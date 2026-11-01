// Package docker запускает сайты клиентов: собирает образ из git-репозитория на ноде и запускает контейнер с лимитами.
// Работает через консольный docker и git, без SDK: у них стабильный интерфейс и ничего не нужно подключать.
package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	keepImages    = 5 // сколько последних образов сайта хранится для отката
	buildTimeout  = 15 * time.Minute
	healthTimeout = 60 * time.Second
	logTail       = 32 << 10 // сколько байт журнала сборки возвращаем
)

var (
	idRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	branchRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,99}$`)
	envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	urlRe    = regexp.MustCompile(`^https://[A-Za-z0-9._~:/?#@!$&'()*+,;=%-]+$`)
)

// Driver — работа с docker и git на ноде.
type Driver struct {
	Docker, Git string
	// AllowLocal разрешает репозитории не только по https. Только для тестов: в боевой работе клонировать с диска ноды нельзя.
	AllowLocal bool
	// HealthWait — сколько ждать, пока приложение откроет порт.
	HealthWait time.Duration
}

func New() *Driver { return &Driver{Docker: "docker", Git: "git", HealthWait: healthTimeout} }

// DeploySpec — что развернуть. Если ImageTag задан, сборка пропускается: это откат на ранее собранную версию.
type DeploySpec struct {
	SiteID, DeployID, RepoURL, Branch string
	Env                               map[string]string
	CPUMilli, MemoryMB, Pids          int
	ContainerPort                     int // 0: определить по стеку или EXPOSE
	HostPort                          int
	BindIP                            string
	ImageTag                          string
}

type DeployResult struct {
	ImageTag      string
	Stack         string
	Commit        string
	ContainerPort int
	Log           string
}

func (s *DeploySpec) validate(allowLocal bool) error {
	switch {
	case !idRe.MatchString(s.SiteID) || !idRe.MatchString(s.DeployID):
		return errors.New("неверный идентификатор")
	case s.ImageTag == "" && !allowLocal && !urlRe.MatchString(s.RepoURL):
		return errors.New("репозиторий должен быть адресом https")
	case s.ImageTag == "" && !branchRe.MatchString(s.Branch):
		return errors.New("неверное имя ветки")
	case s.CPUMilli < 100 || s.MemoryMB < 64 || s.Pids < 16:
		return errors.New("лимиты ресурсов слишком малы")
	case s.HostPort < 1024 || s.HostPort > 65535 || s.ContainerPort < 0 || s.ContainerPort > 65535:
		return errors.New("неверный порт")
	case net.ParseIP(s.BindIP) == nil:
		return errors.New("неверный адрес привязки")
	}
	if s.ImageTag != "" && !strings.HasPrefix(s.ImageTag, imageRepo(s.SiteID)+":") {
		return errors.New("образ не принадлежит этому сайту")
	}
	for k, v := range s.Env {
		if !envKeyRe.MatchString(k) || strings.ContainsAny(v, "\n\r\x00") {
			return fmt.Errorf("неверная переменная %q", k)
		}
	}
	return nil
}

func imageRepo(site string) string          { return "zf-site-" + site }
func containerName(site, dep string) string { return "zf-site-" + site + "-" + dep }

// capture запускает команду и возвращает объединённый вывод; при ошибке к ней добавляется хвост вывода.
func capture(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	out := tail(buf.String(), logTail)
	if err != nil {
		return out, fmt.Errorf("%s %s: %w\n%s", name, args[0], err, tail(out, 2000))
	}
	return out, nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// Deploy собирает (или берёт готовый) образ, запускает новый контейнер рядом со старым, ждёт, пока он откроет порт,
// и только потом убирает старый. Если новая версия не поднялась, старая продолжает работать.
func (d *Driver) Deploy(ctx context.Context, s DeploySpec) (DeployResult, error) {
	var res DeployResult
	if err := s.validate(d.AllowLocal); err != nil {
		return res, err
	}
	res.ImageTag = s.ImageTag
	res.ContainerPort = s.ContainerPort

	if s.ImageTag == "" {
		bctx, cancel := context.WithTimeout(ctx, buildTimeout)
		defer cancel()
		if err := d.build(bctx, &s, &res); err != nil {
			return res, err
		}
	} else if _, err := capture(ctx, "", nil, d.Docker, "image", "inspect", s.ImageTag); err != nil {
		return res, errors.New("образ этой версии уже удалён")
	}
	if res.ContainerPort == 0 {
		res.ContainerPort = 3000
	}

	name := containerName(s.SiteID, s.DeployID)
	_, _ = capture(ctx, "", nil, d.Docker, "rm", "-f", name) // остаток прерванной попытки
	if err := d.run(ctx, s, name, res); err != nil {
		_, _ = capture(ctx, "", nil, d.Docker, "rm", "-f", name)
		return res, err
	}
	if err := d.waitHealthy(ctx, name, s.BindIP, s.HostPort); err != nil {
		logs, _ := capture(ctx, "", nil, d.Docker, "logs", "--tail", "50", name)
		_, _ = capture(ctx, "", nil, d.Docker, "rm", "-f", name)
		res.Log += "\n--- журнал контейнера ---\n" + logs
		return res, err
	}
	d.removeOthers(ctx, s.SiteID, name)
	d.pruneImages(ctx, s.SiteID)
	return res, nil
}

func (d *Driver) build(ctx context.Context, s *DeploySpec, res *DeployResult) error {
	dir, err := os.MkdirTemp("", "zf-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	env := []string{"GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=https"}
	if d.AllowLocal {
		env[1] = "GIT_ALLOW_PROTOCOL=https:file"
	}
	logs, err := capture(ctx, "", env, d.Git, "clone", "--depth", "1", "--branch", s.Branch, "--", s.RepoURL, dir)
	res.Log = logs
	if err != nil {
		return fmt.Errorf("не удалось скачать репозиторий: %w", err)
	}
	if out, err := capture(ctx, dir, env, d.Git, "rev-parse", "HEAD"); err == nil {
		res.Commit = strings.TrimSpace(out)
	}

	stack, port, generated, err := prepareDockerfile(dir)
	if err != nil {
		return err
	}
	res.Stack = stack
	if s.ContainerPort == 0 {
		res.ContainerPort = port
	}
	res.ImageTag = imageRepo(s.SiteID) + ":" + s.DeployID
	if generated {
		res.Log += "\nDockerfile создан автоматически для стека " + stack
	}
	out, err := capture(ctx, dir, nil, d.Docker, "build", "--pull=false", "--label", "zf.site="+s.SiteID, "-t", res.ImageTag, ".")
	res.Log += "\n" + out
	if err != nil {
		return fmt.Errorf("сборка не удалась: %w", err)
	}
	return nil
}

func (d *Driver) run(ctx context.Context, s DeploySpec, name string, res DeployResult) error {
	envFile, err := os.CreateTemp("", "zf-env-")
	if err != nil {
		return err
	}
	defer os.Remove(envFile.Name())
	var b strings.Builder
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k != "PORT" { // порт задаёт платформа
			fmt.Fprintf(&b, "%s=%s\n", k, s.Env[k])
		}
	}
	fmt.Fprintf(&b, "PORT=%d\n", res.ContainerPort)
	if _, err := envFile.WriteString(b.String()); err != nil {
		return err
	}
	_ = envFile.Close()

	// Значения переменных идут через файл, а не аргументы: так их не видно в списке процессов.
	// Лимиты: ядра, память без swap, число процессов. Без привилегий и без лишних capability.
	_, err = capture(ctx, "", nil, d.Docker, "run", "-d", "--name", name,
		"--label", "zf.site="+s.SiteID, "--label", "zf.deploy="+s.DeployID,
		"--restart", "unless-stopped",
		"--cpus", strconv.FormatFloat(float64(s.CPUMilli)/1000, 'f', 3, 64),
		"--memory", fmt.Sprintf("%dm", s.MemoryMB), "--memory-swap", fmt.Sprintf("%dm", s.MemoryMB),
		"--pids-limit", strconv.Itoa(s.Pids),
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--env-file", envFile.Name(),
		"-p", fmt.Sprintf("%s:%d:%d", s.BindIP, s.HostPort, res.ContainerPort),
		resImage(s, res))
	if err != nil {
		return fmt.Errorf("контейнер не запустился: %w", err)
	}
	return nil
}

func resImage(s DeploySpec, res DeployResult) string {
	if s.ImageTag != "" {
		return s.ImageTag
	}
	return res.ImageTag
}

// listening проверяет, что за портом действительно кто-то слушает. Простого подключения мало: docker принимает
// соединение сам и закрывает его, если внутри никто не слушает. Поэтому после подключения ждём: живое приложение
// молчит, пока не получит запрос (или присылает приветствие), а закрытое соединение означает «ещё не готово».
func listening(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
	n, err := c.Read(make([]byte, 1))
	var ne net.Error
	return n > 0 || (errors.As(err, &ne) && ne.Timeout())
}

// waitHealthy ждёт, пока приложение откроет порт и проработает без перезапусков две проверки подряд.
func (d *Driver) waitHealthy(ctx context.Context, name, ip string, port int) error {
	wait := d.HealthWait
	if wait == 0 {
		wait = healthTimeout
	}
	deadline := time.Now().Add(wait)
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	ok := 0
	for time.Now().Before(deadline) {
		out, err := capture(ctx, "", nil, d.Docker, "inspect", "-f", "{{.State.Running}} {{.RestartCount}}", name)
		f := strings.Fields(out)
		if err != nil || len(f) != 2 || f[0] != "true" || f[1] != "0" {
			return errors.New("приложение завершилось сразу после запуска")
		}
		if listening(addr) {
			if ok++; ok >= 2 {
				return nil
			}
		} else {
			ok = 0
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(700 * time.Millisecond):
		}
	}
	return fmt.Errorf("приложение не открыло порт за %s", wait)
}

func (d *Driver) containers(ctx context.Context, siteID string, all bool) []string {
	args := []string{"ps", "--filter", "label=zf.site=" + siteID, "--format", "{{.Names}}"}
	if all {
		args = append(args, "-a")
	}
	out, err := capture(ctx, "", nil, d.Docker, args...)
	if err != nil {
		return nil
	}
	return strings.Fields(out)
}

func (d *Driver) removeOthers(ctx context.Context, siteID, keep string) {
	for _, c := range d.containers(ctx, siteID, true) {
		if c != keep {
			_, _ = capture(ctx, "", nil, d.Docker, "rm", "-f", c)
		}
	}
}

// pruneImages оставляет последние keepImages образов сайта; образы работающих контейнеров не трогаем.
func (d *Driver) pruneImages(ctx context.Context, siteID string) {
	out, err := capture(ctx, "", nil, d.Docker, "image", "ls", imageRepo(siteID), "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return
	}
	tags := strings.Fields(out) // docker отдаёт от новых к старым
	for i, t := range tags {
		if i >= keepImages {
			_, _ = capture(ctx, "", nil, d.Docker, "rmi", t)
		}
	}
}

// Images возвращает образы сайта от новых к старым (для отката).
func (d *Driver) Images(ctx context.Context, siteID string) []string {
	out, err := capture(ctx, "", nil, d.Docker, "image", "ls", imageRepo(siteID), "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return nil
	}
	return strings.Fields(out)
}

func (d *Driver) Stop(ctx context.Context, siteID string) error {
	if !idRe.MatchString(siteID) {
		return errors.New("неверный идентификатор")
	}
	for _, c := range d.containers(ctx, siteID, false) {
		if _, err := capture(ctx, "", nil, d.Docker, "stop", "-t", "10", c); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) Start(ctx context.Context, siteID string) error {
	if !idRe.MatchString(siteID) {
		return errors.New("неверный идентификатор")
	}
	cs := d.containers(ctx, siteID, true)
	if len(cs) == 0 {
		return errors.New("у сайта нет контейнера: нужен новый деплой")
	}
	for _, c := range cs {
		if _, err := capture(ctx, "", nil, d.Docker, "start", c); err != nil {
			return err
		}
	}
	return nil
}

// Remove удаляет контейнеры и образы сайта.
func (d *Driver) Remove(ctx context.Context, siteID string) error {
	if !idRe.MatchString(siteID) {
		return errors.New("неверный идентификатор")
	}
	for _, c := range d.containers(ctx, siteID, true) {
		_, _ = capture(ctx, "", nil, d.Docker, "rm", "-f", c)
	}
	for _, t := range d.Images(ctx, siteID) {
		_, _ = capture(ctx, "", nil, d.Docker, "rmi", "-f", t)
	}
	return nil
}

func (d *Driver) Logs(ctx context.Context, siteID string, lines int) (string, error) {
	if !idRe.MatchString(siteID) {
		return "", errors.New("неверный идентификатор")
	}
	cs := d.containers(ctx, siteID, true)
	if len(cs) == 0 {
		return "", errors.New("у сайта нет контейнера")
	}
	return capture(ctx, "", nil, d.Docker, "logs", "--tail", strconv.Itoa(min(max(lines, 1), 1000)), cs[0])
}

// Status возвращает running, stopped или none.
func (d *Driver) Status(ctx context.Context, siteID string) string {
	switch {
	case len(d.containers(ctx, siteID, false)) > 0:
		return "running"
	case len(d.containers(ctx, siteID, true)) > 0:
		return "stopped"
	}
	return "none"
}
