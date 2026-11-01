package docker

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	ok := DeploySpec{SiteID: "s1", DeployID: "d1", RepoURL: "https://github.com/a/b", Branch: "main", CPUMilli: 500, MemoryMB: 256, Pids: 128, HostPort: 20001, BindIP: "10.99.0.1"}
	if err := ok.validate(false); err != nil {
		t.Fatalf("корректный запрос: %v", err)
	}
	for name, mutate := range map[string]func(*DeploySpec){
		"репозиторий по ssh":     func(s *DeploySpec) { s.RepoURL = "git@github.com:a/b.git" },
		"файл на диске ноды":     func(s *DeploySpec) { s.RepoURL = "file:///etc" },
		"ext-протокол":           func(s *DeploySpec) { s.RepoURL = "ext::sh -c id" },
		"ветка как опция":        func(s *DeploySpec) { s.Branch = "--upload-pack=x" },
		"имя сайта с символами":  func(s *DeploySpec) { s.SiteID = "../x" },
		"нет лимитов":            func(s *DeploySpec) { s.MemoryMB = 0 },
		"привилегированный порт": func(s *DeploySpec) { s.HostPort = 80 },
		"плохой адрес":           func(s *DeploySpec) { s.BindIP = "localhost; id" },
		"ключ переменной":        func(s *DeploySpec) { s.Env = map[string]string{"A B": "1"} },
		"перевод строки в Env":   func(s *DeploySpec) { s.Env = map[string]string{"A": "x\nB=2"} },
		"чужой образ при откате": func(s *DeploySpec) { s.ImageTag = "zf-site-other:d1" },
	} {
		s := ok
		mutate(&s)
		if err := s.validate(false); err == nil {
			t.Errorf("%s: должно отклоняться", name)
		}
	}
}

func TestDetectStack(t *testing.T) {
	write := func(files map[string]string) string {
		dir := t.TempDir()
		for n, c := range files {
			_ = os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644)
		}
		return dir
	}
	cases := []struct {
		name  string
		files map[string]string
		stack string
		port  int
		want  string // подстрока в Dockerfile
	}{
		{"next", map[string]string{"package.json": `{"dependencies":{"next":"15"}}`}, "Next.js", 3000, "npm run build"},
		{"node со start", map[string]string{"package.json": `{"scripts":{"start":"node a.js"}}`}, "Node.js", 3000, `CMD ["npm", "start"]`},
		{"node без start", map[string]string{"package.json": `{"main":"srv.js"}`, "srv.js": ""}, "Node.js", 3000, `CMD ["node", "srv.js"]`},
		{"python", map[string]string{"requirements.txt": "flask", "app.py": ""}, "Python", 8000, `CMD ["python", "app.py"]`},
	}
	for _, c := range cases {
		stack, port, content, err := detectStack(write(c.files))
		if err != nil || stack != c.stack || port != c.port || !strings.Contains(content, c.want) {
			t.Errorf("%s: %s %d %v", c.name, stack, port, err)
		}
	}
	for name, files := range map[string]map[string]string{
		"пусто":            {"README.md": ""},
		"node без запуска": {"package.json": `{}`},
		"python без файла": {"requirements.txt": "x"},
		"битый package":    {"package.json": `{`},
	} {
		if _, _, _, err := detectStack(write(files)); err == nil {
			t.Errorf("%s: ждали ошибку", name)
		}
	}
	// Готовый Dockerfile не трогаем, порт из EXPOSE.
	dir := write(map[string]string{"Dockerfile": "FROM x\nEXPOSE 8080\n"})
	stack, port, gen, err := prepareDockerfile(dir)
	if err != nil || stack != "Dockerfile" || port != 8080 || gen {
		t.Errorf("свой Dockerfile: %s %d %v %v", stack, port, gen, err)
	}
}

// --- настоящий docker ---

func needDocker(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("короткий режим")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker недоступен")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git недоступен")
	}
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func commit(t *testing.T, repo, dockerfile string) {
	t.Helper()
	_ = os.WriteFile(filepath.Join(repo, "Dockerfile"), []byte(dockerfile), 0o644)
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "x", "--allow-empty")
}

func get(t *testing.T, port int) string {
	t.Helper()
	var last error
	for i := 0; i < 20; i++ {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/index.html", port))
		if err == nil {
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return strings.TrimSpace(string(b))
		}
		last = err
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("порт %d не отвечает: %v", port, last)
	return ""
}

const goodApp = `FROM busybox
EXPOSE 3000
CMD ["sh", "-c", "echo \"%s $GREETING\" > /index.html && exec httpd -f -p $PORT -h /"]
`

func TestDeployRollbackAndFailureSafety(t *testing.T) {
	needDocker(t)
	ctx := context.Background()
	d := New()
	d.AllowLocal = true
	d.HealthWait = 20 * time.Second

	repo := t.TempDir()
	git(t, repo, "init", "-b", "main")
	commit(t, repo, fmt.Sprintf(goodApp, "v1"))

	site := fmt.Sprintf("t%d", time.Now().UnixNano()%1_000_000_000)
	t.Cleanup(func() { _ = d.Remove(ctx, site) })
	spec := func(dep string, port int) DeploySpec {
		return DeploySpec{SiteID: site, DeployID: dep, RepoURL: repo, Branch: "main", Env: map[string]string{"GREETING": "hello"},
			CPUMilli: 500, MemoryMB: 128, Pids: 64, HostPort: port, BindIP: "127.0.0.1"}
	}

	// Первый деплой: сборка, запуск, переменные и порт дошли, лимиты выставлены.
	p1 := freePort(t)
	r1, err := d.Deploy(ctx, spec("d1", p1))
	if err != nil {
		t.Fatalf("деплой 1: %v\n%s", err, r1.Log)
	}
	if r1.Stack != "Dockerfile" || r1.ContainerPort != 3000 || len(r1.Commit) != 40 {
		t.Errorf("результат: %+v", r1)
	}
	if got := get(t, p1); got != "v1 hello" {
		t.Fatalf("ответ v1: %q", got)
	}
	out, _ := exec.Command("docker", "inspect", "-f", "{{.HostConfig.Memory}} {{.HostConfig.NanoCpus}} {{.HostConfig.PidsLimit}} {{.HostConfig.CapDrop}}", containerName(site, "d1")).Output()
	if got := strings.TrimSpace(string(out)); got != fmt.Sprintf("%d 500000000 64 [ALL]", 128<<20) {
		t.Errorf("лимиты контейнера: %q", got)
	}
	if st := d.Status(ctx, site); st != "running" {
		t.Errorf("статус: %s", st)
	}

	// Второй деплой на другом порту; старый контейнер убирается только после успеха нового.
	commit(t, repo, fmt.Sprintf(goodApp, "v2"))
	p2 := freePort(t)
	if _, err := d.Deploy(ctx, spec("d2", p2)); err != nil {
		t.Fatal(err)
	}
	if got := get(t, p2); got != "v2 hello" {
		t.Fatalf("ответ v2: %q", got)
	}
	if cs := d.containers(ctx, site, true); len(cs) != 1 || cs[0] != containerName(site, "d2") {
		t.Errorf("после деплоя должен остаться один контейнер: %v", cs)
	}

	// Сломанный деплой: приложение падает сразу. Работающая v2 не страдает, мусор убран.
	commit(t, repo, "FROM busybox\nCMD [\"sh\", \"-c\", \"echo boom; exit 1\"]\n")
	p3 := freePort(t)
	r3, err := d.Deploy(ctx, spec("d3", p3))
	if err == nil {
		t.Fatal("падающее приложение не должно считаться успешным деплоем")
	}
	if !strings.Contains(r3.Log, "boom") {
		t.Errorf("в журнале должна быть причина: %q", r3.Log)
	}
	if got := get(t, p2); got != "v2 hello" {
		t.Errorf("старая версия должна продолжать работать: %q", got)
	}
	if cs := d.containers(ctx, site, true); len(cs) != 1 || cs[0] != containerName(site, "d2") {
		t.Errorf("контейнер неудачной версии должен быть удалён: %v", cs)
	}

	// Откат на образ первой версии без пересборки.
	p4 := freePort(t)
	rb := spec("d4", p4)
	rb.ImageTag = imageRepo(site) + ":d1"
	if _, err := d.Deploy(ctx, rb); err != nil {
		t.Fatalf("откат: %v", err)
	}
	if got := get(t, p4); got != "v1 hello" {
		t.Errorf("после отката ждали v1: %q", got)
	}
	bad := spec("d5", freePort(t))
	bad.ImageTag = imageRepo(site) + ":нет-такого"
	if _, err := d.Deploy(ctx, bad); err == nil {
		t.Error("откат на несуществующий образ должен отклоняться")
	}

	// Остановка, запуск, журнал, удаление.
	if err := d.Stop(ctx, site); err != nil || d.Status(ctx, site) != "stopped" {
		t.Fatalf("остановка: %v %s", err, d.Status(ctx, site))
	}
	if err := d.Start(ctx, site); err != nil || d.Status(ctx, site) != "running" {
		t.Fatalf("запуск: %v", err)
	}
	if _, err := d.Logs(ctx, site, 10); err != nil {
		t.Errorf("журнал: %v", err)
	}
	if err := d.Remove(ctx, site); err != nil || d.Status(ctx, site) != "none" || len(d.Images(ctx, site)) != 0 {
		t.Errorf("удаление: %v %s %v", err, d.Status(ctx, site), d.Images(ctx, site))
	}
}

func TestAutoDockerfileBuildsAndRuns(t *testing.T) {
	needDocker(t)
	ctx := context.Background()
	d := New()
	d.AllowLocal = true
	d.HealthWait = 30 * time.Second

	// Без Dockerfile: стек определяется по package.json. Образ node тянется из сети, поэтому тест нужен именно с настоящим docker.
	repo := t.TempDir()
	git(t, repo, "init", "-b", "main")
	_ = os.WriteFile(filepath.Join(repo, "package.json"), []byte(`{"name":"x","scripts":{"start":"node server.js"}}`), 0o644)
	_ = os.WriteFile(filepath.Join(repo, "server.js"), []byte(`require("http").createServer((q,r)=>r.end("node ok "+process.env.GREETING)).listen(process.env.PORT)`), 0o644)
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "x")

	site := fmt.Sprintf("a%d", time.Now().UnixNano()%1_000_000_000)
	t.Cleanup(func() { _ = d.Remove(ctx, site) })
	port := freePort(t)
	res, err := d.Deploy(ctx, DeploySpec{SiteID: site, DeployID: "d1", RepoURL: repo, Branch: "main", Env: map[string]string{"GREETING": "hi"},
		CPUMilli: 500, MemoryMB: 256, Pids: 128, HostPort: port, BindIP: "127.0.0.1"})
	if err != nil {
		t.Fatalf("%v\n%s", err, res.Log)
	}
	if res.Stack != "Node.js" || res.ContainerPort != 3000 {
		t.Errorf("стек: %+v", res)
	}
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if b, _ := io.ReadAll(resp.Body); string(b) != "node ok hi" {
		t.Errorf("ответ: %q", b)
	}
}
