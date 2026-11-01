package docker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDBValidate(t *testing.T) {
	ok := DBSpec{DBID: "d1", Engine: "postgres", Version: "17", DBName: "app", User: "app", Password: "abcdefghij0123456789", CPUMilli: 500, MemoryMB: 256, Pids: 128, HostPort: 30000, BindIP: "10.99.0.1"}
	if err := ok.validate(); err != nil {
		t.Fatalf("корректный запрос: %v", err)
	}
	for name, mutate := range map[string]func(*DBSpec){
		"чужой образ как версия": func(s *DBSpec) { s.Version = "latest" },
		"неизвестный движок":     func(s *DBSpec) { s.Engine = "mongo" },
		"имя базы с символами":   func(s *DBSpec) { s.DBName = "app; drop" },
		"пользователь":           func(s *DBSpec) { s.User = "Root" },
		"короткий пароль":        func(s *DBSpec) { s.Password = "short" },
		"пароль с символами":     func(s *DBSpec) { s.Password = "abcdefghij012345678-" },
		"идентификатор":          func(s *DBSpec) { s.DBID = "../x" },
		"привилегированный порт": func(s *DBSpec) { s.HostPort = 80 },
		"нет лимитов":            func(s *DBSpec) { s.MemoryMB = 0 },
		"адрес привязки":         func(s *DBSpec) { s.BindIP = "0.0.0.0.1" },
	} {
		s := ok
		mutate(&s)
		if err := s.validate(); err == nil {
			t.Errorf("%s: должно отклоняться", name)
		}
	}
	// У Redis нет имён базы и пользователя.
	r := ok
	r.Engine, r.Version, r.DBName, r.User = "redis", "7.4", "", ""
	if err := r.validate(); err != nil {
		t.Errorf("redis: %v", err)
	}
}

func volumeExists(name string) bool {
	return exec.Command("docker", "volume", "inspect", name).Run() == nil
}

func dockerExec(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out, err := exec.Command("docker", append([]string{"exec"}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func TestPostgresLifecycle(t *testing.T) {
	needDocker(t)
	ctx := context.Background()
	d := New()
	d.HealthWait = 90 * time.Second
	id := fmt.Sprintf("p%d", time.Now().UnixNano()%1_000_000_000)
	t.Cleanup(func() { _ = d.RemoveDB(ctx, id) })
	port := freePort(t)
	pass := "Pg" + strings.Repeat("x", 18)

	if err := d.CreateDB(ctx, DBSpec{DBID: id, Engine: "postgres", Version: "17", DBName: "app", User: "appuser", Password: pass,
		CPUMilli: 500, MemoryMB: 256, Pids: 128, HostPort: port, BindIP: "127.0.0.1"}); err != nil {
		t.Fatalf("создание: %v", err)
	}
	if d.DBStatus(ctx, id) != "running" {
		t.Fatal("статус")
	}
	c := dbContainer(id)
	// Пароль действует, чужой отклоняется. Подключаемся по адресу контейнера в сети docker, как подключались бы снаружи:
	// на localhost образ postgres пускает без пароля.
	remote := func(password string) (string, error) {
		return dockerExec(t, "-e", "PGPASSWORD="+password, c, "sh", "-c", `psql -h "$(hostname -i)" -U appuser -d app -tAc "select 1"`)
	}
	if out, err := remote(pass); err != nil || out != "1" {
		t.Fatalf("вход с паролем: %q %v", out, err)
	}
	if _, err := remote("wrong"); err == nil {
		t.Error("с неверным паролем входить нельзя")
	}
	// Лимиты и ограничения.
	out, _ := exec.Command("docker", "inspect", "-f", "{{.HostConfig.Memory}} {{.HostConfig.NanoCpus}} {{.HostConfig.PidsLimit}}", c).Output()
	if got := strings.TrimSpace(string(out)); got != fmt.Sprintf("%d 500000000 128", 256<<20) {
		t.Errorf("лимиты: %q", got)
	}

	// Данные переживают остановку и запуск.
	if _, err := dockerExec(t, "-e", "PGPASSWORD="+pass, c, "psql", "-h", "127.0.0.1", "-U", "appuser", "-d", "app", "-c", "create table t(v text); insert into t values ('keep me')"); err != nil {
		t.Fatal(err)
	}
	if err := d.StopDB(ctx, id); err != nil || d.DBStatus(ctx, id) != "stopped" {
		t.Fatalf("остановка: %v %s", err, d.DBStatus(ctx, id))
	}
	if err := d.StartDB(ctx, id); err != nil {
		t.Fatal(err)
	}
	var got string
	for i := 0; i < 40; i++ {
		got, _ = dockerExec(t, "-e", "PGPASSWORD="+pass, c, "psql", "-h", "127.0.0.1", "-U", "appuser", "-d", "app", "-tAc", "select v from t")
		if got == "keep me" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if got != "keep me" {
		t.Errorf("данные после перезапуска: %q", got)
	}
	if mb, err := d.DBUsageMB(ctx, id, "postgres", "17"); err != nil || mb < 1 {
		t.Errorf("занято места: %d %v", mb, err)
	}
	if logs, err := d.DBLogs(ctx, id, 10); err != nil || logs == "" {
		t.Errorf("журнал: %q %v", logs, err)
	}
	// Повторное создание с тем же идентификатором не затирает данные.
	if err := d.CreateDB(ctx, DBSpec{DBID: id, Engine: "postgres", Version: "17", DBName: "app", User: "appuser", Password: pass,
		CPUMilli: 500, MemoryMB: 256, Pids: 128, HostPort: freePort(t), BindIP: "127.0.0.1"}); err == nil {
		t.Error("повторное создание должно отклоняться")
	}

	if err := d.RemoveDB(ctx, id); err != nil || d.DBStatus(ctx, id) != "none" || volumeExists(dbVolume(id)) {
		t.Errorf("удаление: %v %s volume=%v", err, d.DBStatus(ctx, id), volumeExists(dbVolume(id)))
	}
}

func TestCreateDBFailureCleansVolume(t *testing.T) {
	needDocker(t)
	ctx := context.Background()
	d := New()
	id := fmt.Sprintf("f%d", time.Now().UnixNano()%1_000_000_000)
	t.Cleanup(func() { _ = d.RemoveDB(ctx, id) })

	// База не успевает подняться за отведённое время (первичная инициализация дольше секунды): том и контейнер убираются.
	d.HealthWait = time.Second
	err := d.CreateDB(ctx, DBSpec{DBID: id, Engine: "postgres", Version: "17", DBName: "app", User: "appuser", Password: "Pg" + strings.Repeat("y", 18),
		CPUMilli: 500, MemoryMB: 256, Pids: 128, HostPort: freePort(t), BindIP: "127.0.0.1"})
	if err == nil {
		t.Fatal("ждали ошибку: база не успела подняться")
	}
	if volumeExists(dbVolume(id)) || d.DBStatus(ctx, id) != "none" {
		t.Error("после неудачного создания не должно остаться ни тома, ни контейнера")
	}
}

func TestRedisRequiresPassword(t *testing.T) {
	needDocker(t)
	ctx := context.Background()
	d := New()
	d.HealthWait = 90 * time.Second
	id := fmt.Sprintf("r%d", time.Now().UnixNano()%1_000_000_000)
	t.Cleanup(func() { _ = d.RemoveDB(ctx, id) })
	pass := "Rd" + strings.Repeat("z", 18)
	if err := d.CreateDB(ctx, DBSpec{DBID: id, Engine: "redis", Version: "7.4", Password: pass, CPUMilli: 300, MemoryMB: 128, Pids: 64, HostPort: freePort(t), BindIP: "127.0.0.1"}); err != nil {
		t.Fatalf("создание: %v", err)
	}
	c := dbContainer(id)
	if out, err := dockerExec(t, c, "redis-cli", "-a", pass, "--no-auth-warning", "ping"); err != nil || out != "PONG" {
		t.Errorf("с паролем: %q %v", out, err)
	}
	if out, _ := dockerExec(t, c, "redis-cli", "ping"); !strings.Contains(out, "NOAUTH") {
		t.Errorf("без пароля должен быть отказ: %q", out)
	}
}

// fakeS3 хранит загруженное в памяти: проверяет, что агент грузит с длиной, и отдаёт загруженное обратно.
type fakeS3 struct {
	mu   sync.Mutex
	data []byte
	puts int
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		if r.ContentLength <= 0 {
			http.Error(w, "MissingContentLength", http.StatusLengthRequired)
			return
		}
		f.data, _ = io.ReadAll(r.Body)
		f.puts++
	case http.MethodGet:
		if f.data == nil {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(f.data)
	}
}

func TestPostgresBackupAndRestore(t *testing.T) {
	needDocker(t)
	ctx := context.Background()
	d := New()
	d.AllowLocal = true // тестовое хранилище по http
	d.HealthWait = 90 * time.Second
	s3 := &fakeS3{}
	srv := httptest.NewServer(s3)
	defer srv.Close()
	id := fmt.Sprintf("b%d", time.Now().UnixNano()%1_000_000_000)
	t.Cleanup(func() { _ = d.RemoveDB(ctx, id) })
	pass := "Bk" + strings.Repeat("q", 18)
	if err := d.CreateDB(ctx, DBSpec{DBID: id, Engine: "postgres", Version: "17", DBName: "app", User: "appuser", Password: pass,
		CPUMilli: 500, MemoryMB: 256, Pids: 128, HostPort: freePort(t), BindIP: "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	c := dbContainer(id)
	psql := func(sql string) (string, error) {
		return dockerExec(t, "-e", "PGPASSWORD="+pass, c, "sh", "-c", `psql -h "$(hostname -i)" -U appuser -d app -tAc "`+sql+`"`)
	}
	if out, err := psql("create table notes(v text); insert into notes values ('before backup')"); err != nil {
		t.Fatalf("%s %v", out, err)
	}

	size, err := d.DumpDB(ctx, id, "postgres", srv.URL+"/bucket/dump")
	if err != nil || size <= 0 {
		t.Fatalf("дамп: %d %v", size, err)
	}
	if !strings.HasPrefix(string(s3.data), "PGDMP") || int64(len(s3.data)) != size {
		t.Errorf("в хранилище не дамп postgres: %q (%d из %d байт)", s3.data[:min(8, len(s3.data))], len(s3.data), size)
	}

	// Данные испортились после копии: восстановление возвращает состояние на момент копии.
	if out, err := psql("delete from notes; insert into notes values ('after backup')"); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	if err := d.RestoreDB(ctx, id, "postgres", srv.URL+"/bucket/dump"); err != nil {
		t.Fatalf("восстановление: %v", err)
	}
	if out, _ := psql("select v from notes"); out != "before backup" {
		t.Errorf("после восстановления: %q", out)
	}

	// Ограничения: только https в работе, redis не восстанавливаем, плохой адрес.
	strict := New()
	if _, err := strict.DumpDB(ctx, id, "postgres", "http://example.com/x"); err == nil {
		t.Error("http-хранилище в работе должно отклоняться")
	}
	if err := d.RestoreDB(ctx, id, "redis", srv.URL+"/bucket/dump"); err == nil {
		t.Error("восстановление redis пока не поддерживается")
	}
	if _, err := d.DumpDB(ctx, "../x", "postgres", srv.URL); err == nil {
		t.Error("неверный идентификатор")
	}
	// Хранилище недоступно: ошибка, а не зависание.
	srv.Close()
	if _, err := d.DumpDB(ctx, id, "postgres", srv.URL+"/bucket/dump"); err == nil {
		t.Error("недоступное хранилище должно давать ошибку")
	}
}

func TestRedisDump(t *testing.T) {
	needDocker(t)
	ctx := context.Background()
	d := New()
	d.AllowLocal = true
	d.HealthWait = 90 * time.Second
	s3 := &fakeS3{}
	srv := httptest.NewServer(s3)
	defer srv.Close()
	id := fmt.Sprintf("rd%d", time.Now().UnixNano()%1_000_000_000)
	t.Cleanup(func() { _ = d.RemoveDB(ctx, id) })
	pass := "Rd" + strings.Repeat("k", 18)
	if err := d.CreateDB(ctx, DBSpec{DBID: id, Engine: "redis", Version: "7.4", Password: pass, CPUMilli: 300, MemoryMB: 128, Pids: 64, HostPort: freePort(t), BindIP: "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := dockerExec(t, dbContainer(id), "redis-cli", "-a", pass, "--no-auth-warning", "set", "k", "v"); err != nil {
		t.Fatal(err)
	}
	size, err := d.DumpDB(ctx, id, "redis", srv.URL+"/b/d")
	if err != nil || size <= 0 || !strings.HasPrefix(string(s3.data), "REDIS") {
		t.Fatalf("дамп redis: %d %v %q", size, err, s3.data[:min(8, len(s3.data))])
	}
}
