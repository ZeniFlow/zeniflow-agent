package docker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// engineSpec описывает, как запускать движок БД. Образы фиксированы: клиент выбирает только идентификатор версии,
// произвольный образ запустить нельзя.
type engineSpec struct {
	Image string
	Port  int
	Data  string // каталог данных внутри контейнера
}

var engines = map[string]map[string]engineSpec{
	"postgres": {
		"17": {"postgres:17-alpine", 5432, "/var/lib/postgresql/data"},
		"16": {"postgres:16-alpine", 5432, "/var/lib/postgresql/data"},
	},
	"mysql": {
		"mysql-8.4":    {"mysql:8.4", 3306, "/var/lib/mysql"},
		"mariadb-11.4": {"mariadb:11.4", 3306, "/var/lib/mysql"},
	},
	"redis": {
		"7.4": {"redis:7.4-alpine", 6379, "/data"},
	},
}

// DBSpec — что развернуть.
type DBSpec struct {
	DBID, Engine, Version    string
	DBName, User, Password   string
	CPUMilli, MemoryMB, Pids int
	HostPort                 int
	BindIP                   string
}

var (
	dbNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	passRe   = regexp.MustCompile(`^[A-Za-z0-9]{16,64}$`)
)

func dbContainer(id string) string { return "zf-db-" + id }
func dbVolume(id string) string    { return "zf-db-" + id }

func (s *DBSpec) engine() (engineSpec, error) {
	e, ok := engines[s.Engine][s.Version]
	if !ok {
		return engineSpec{}, errors.New("неизвестный движок или версия")
	}
	return e, nil
}

func (s *DBSpec) validate() error {
	if _, err := s.engine(); err != nil {
		return err
	}
	switch {
	case !idRe.MatchString(s.DBID):
		return errors.New("неверный идентификатор")
	case s.Engine != "redis" && (!dbNameRe.MatchString(s.DBName) || !dbNameRe.MatchString(s.User)):
		return errors.New("неверное имя базы или пользователя")
	case !passRe.MatchString(s.Password):
		return errors.New("пароль должен быть из 16–64 латинских букв и цифр")
	case s.CPUMilli < 100 || s.MemoryMB < 64 || s.Pids < 16:
		return errors.New("лимиты ресурсов слишком малы")
	case s.HostPort < 1024 || s.HostPort > 65535:
		return errors.New("неверный порт")
	case net.ParseIP(s.BindIP) == nil:
		return errors.New("неверный адрес привязки")
	}
	return nil
}

// CreateDB создаёт том и запускает контейнер базы. Если база не поднялась, том тоже удаляется: данных там ещё нет.
func (d *Driver) CreateDB(ctx context.Context, s DBSpec) error {
	if err := s.validate(); err != nil {
		return err
	}
	e, _ := s.engine()
	name, vol := dbContainer(s.DBID), dbVolume(s.DBID)
	if out, _ := capture(ctx, "", nil, d.Docker, "ps", "-a", "--filter", "name=^"+name+"$", "--format", "{{.Names}}"); strings.TrimSpace(out) != "" {
		return errors.New("база с таким идентификатором уже есть")
	}
	if _, err := capture(ctx, "", nil, d.Docker, "volume", "create", "--label", "zf.db="+s.DBID, vol); err != nil {
		return err
	}
	cleanup := func() {
		_, _ = capture(ctx, "", nil, d.Docker, "rm", "-f", name)
		_, _ = capture(ctx, "", nil, d.Docker, "volume", "rm", "-f", vol)
	}

	envFile, err := os.CreateTemp("", "zf-dbenv-")
	if err != nil {
		cleanup()
		return err
	}
	defer os.Remove(envFile.Name())
	var env, cmd []string
	switch s.Engine {
	case "postgres":
		env = []string{"POSTGRES_USER=" + s.User, "POSTGRES_PASSWORD=" + s.Password, "POSTGRES_DB=" + s.DBName}
	case "mysql":
		// Пароль root случайный и нигде не хранится: клиент работает под своим пользователем.
		env = []string{"MYSQL_RANDOM_ROOT_PASSWORD=yes", "MYSQL_USER=" + s.User, "MYSQL_PASSWORD=" + s.Password, "MYSQL_DATABASE=" + s.DBName}
	case "redis":
		env = []string{"REDIS_PASSWORD=" + s.Password}
		cmd = []string{"sh", "-c", `exec redis-server --requirepass "$REDIS_PASSWORD" --appendonly yes --protected-mode yes`}
	}
	if _, err := envFile.WriteString(strings.Join(env, "\n") + "\n"); err != nil {
		cleanup()
		return err
	}
	_ = envFile.Close()

	args := []string{"run", "-d", "--name", name, "--label", "zf.db=" + s.DBID, "--restart", "unless-stopped",
		"--cpus", strconv.FormatFloat(float64(s.CPUMilli)/1000, 'f', 3, 64),
		"--memory", fmt.Sprintf("%dm", s.MemoryMB), "--memory-swap", fmt.Sprintf("%dm", s.MemoryMB),
		"--pids-limit", strconv.Itoa(s.Pids),
		// Образам БД нужно сменить пользователя и владельца каталога данных, остальное отбираем.
		"--cap-drop", "ALL", "--cap-add", "CHOWN", "--cap-add", "SETUID", "--cap-add", "SETGID", "--cap-add", "DAC_OVERRIDE", "--cap-add", "FOWNER",
		"--security-opt", "no-new-privileges",
		"--env-file", envFile.Name(),
		"-v", vol + ":" + e.Data,
		"-p", fmt.Sprintf("%s:%d:%d", s.BindIP, s.HostPort, e.Port),
		e.Image}
	args = append(args, cmd...)
	if _, err := capture(ctx, "", nil, d.Docker, args...); err != nil {
		cleanup()
		return fmt.Errorf("база не запустилась: %w", err)
	}
	if err := d.waitHealthy(ctx, name, s.BindIP, s.HostPort); err != nil {
		logs, _ := capture(ctx, "", nil, d.Docker, "logs", "--tail", "30", name)
		cleanup()
		return fmt.Errorf("%w\n%s", err, tail(logs, 2000))
	}
	return nil
}

func (d *Driver) dbExists(ctx context.Context, id string) bool {
	out, _ := capture(ctx, "", nil, d.Docker, "ps", "-a", "--filter", "label=zf.db="+id, "--format", "{{.Names}}")
	return strings.TrimSpace(out) != ""
}

func (d *Driver) StopDB(ctx context.Context, id string) error {
	if !idRe.MatchString(id) {
		return errors.New("неверный идентификатор")
	}
	_, err := capture(ctx, "", nil, d.Docker, "stop", "-t", "30", dbContainer(id)) // БД нужно время сбросить данные на диск
	return err
}

func (d *Driver) StartDB(ctx context.Context, id string) error {
	if !idRe.MatchString(id) {
		return errors.New("неверный идентификатор")
	}
	_, err := capture(ctx, "", nil, d.Docker, "start", dbContainer(id))
	return err
}

// RemoveDB удаляет контейнер и том с данными. Необратимо: вызывающий обязан убедиться, что клиент это подтвердил.
func (d *Driver) RemoveDB(ctx context.Context, id string) error {
	if !idRe.MatchString(id) {
		return errors.New("неверный идентификатор")
	}
	_, _ = capture(ctx, "", nil, d.Docker, "rm", "-f", dbContainer(id))
	if _, err := capture(ctx, "", nil, d.Docker, "volume", "rm", "-f", dbVolume(id)); err != nil {
		return err
	}
	return nil
}

func (d *Driver) DBLogs(ctx context.Context, id string, lines int) (string, error) {
	if !idRe.MatchString(id) {
		return "", errors.New("неверный идентификатор")
	}
	return capture(ctx, "", nil, d.Docker, "logs", "--tail", strconv.Itoa(min(max(lines, 1), 1000)), dbContainer(id))
}

// DBStatus возвращает running, stopped или none.
func (d *Driver) DBStatus(ctx context.Context, id string) string {
	out, err := capture(ctx, "", nil, d.Docker, "inspect", "-f", "{{.State.Running}}", dbContainer(id))
	switch {
	case err != nil:
		return "none"
	case strings.TrimSpace(out) == "true":
		return "running"
	}
	return "stopped"
}

// DBUsageMB — сколько места занимают данные базы в мегабайтах.
func (d *Driver) DBUsageMB(ctx context.Context, id string, engine, version string) (int, error) {
	e, ok := engines[engine][version]
	if !ok || !idRe.MatchString(id) {
		return 0, errors.New("неверные параметры")
	}
	out, err := capture(ctx, "", nil, d.Docker, "exec", dbContainer(id), "du", "-sm", e.Data)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.Fields(out)[0])
	return n, err
}
