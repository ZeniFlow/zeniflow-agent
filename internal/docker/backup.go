package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

const transferTimeout = 30 * time.Minute

func (d *Driver) checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("неверный адрес хранилища")
	}
	if u.Scheme != "https" && !(d.AllowLocal && u.Scheme == "http") { // в работе только https
		return errors.New("хранилище должно быть доступно по https")
	}
	return nil
}

// dumpCommand — команда внутри контейнера, которая пишет дамп в stdout. Пароли берутся из окружения самого контейнера,
// поэтому ни в аргументах процесса на ноде, ни в команде от панели их нет.
func dumpCommand(engine string) (string, error) {
	switch engine {
	case "postgres":
		return `pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc`, nil
	case "mysql":
		return `MYSQL_PWD="$MYSQL_PASSWORD" "$(command -v mariadb-dump || command -v mysqldump)" -u"$MYSQL_USER" --single-transaction --routines "$MYSQL_DATABASE" | gzip`, nil
	case "redis":
		return `redis-cli -a "$REDIS_PASSWORD" --no-auth-warning --rdb /tmp/zf.rdb >/dev/null 2>&1 && cat /tmp/zf.rdb; rm -f /tmp/zf.rdb`, nil
	}
	return "", errors.New("неизвестный движок")
}

func restoreCommand(engine string) (string, error) {
	switch engine {
	case "postgres":
		return `pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --clean --if-exists --no-owner`, nil
	case "mysql":
		return `gunzip | MYSQL_PWD="$MYSQL_PASSWORD" "$(command -v mariadb || command -v mysql)" -u"$MYSQL_USER" "$MYSQL_DATABASE"`, nil
	}
	return "", errors.New("восстановление для этого движка пока не поддерживается")
}

// DumpDB снимает дамп базы во временный файл и загружает его по ссылке хранилища (PUT). Возвращает размер файла.
func (d *Driver) DumpDB(ctx context.Context, id, engine, putURL string) (int64, error) {
	if !idRe.MatchString(id) {
		return 0, errors.New("неверный идентификатор")
	}
	if err := d.checkURL(putURL); err != nil {
		return 0, err
	}
	script, err := dumpCommand(engine)
	if err != nil {
		return 0, err
	}
	f, err := os.CreateTemp("", "zf-dump-")
	if err != nil {
		return 0, err
	}
	defer os.Remove(f.Name())
	defer f.Close()

	cctx, cancel := context.WithTimeout(ctx, transferTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, d.Docker, "exec", dbContainer(id), "sh", "-c", script)
	var stderr strings.Builder
	cmd.Stdout, cmd.Stderr = f, &stderr
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("дамп не снят: %w: %s", err, tail(stderr.String(), 1500))
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if size == 0 {
		return 0, errors.New("дамп пустой")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(cctx, http.MethodPut, putURL, f)
	if err != nil {
		return 0, err
	}
	req.ContentLength = size // хранилище S3 не принимает загрузку без длины
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("загрузка в хранилище: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return 0, fmt.Errorf("хранилище ответило %d: %s", resp.StatusCode, body)
	}
	return size, nil
}

// RestoreDB скачивает дамп по ссылке (GET) и загружает его в базу, заменяя текущие данные.
func (d *Driver) RestoreDB(ctx context.Context, id, engine, getURL string) error {
	if !idRe.MatchString(id) {
		return errors.New("неверный идентификатор")
	}
	if err := d.checkURL(getURL); err != nil {
		return err
	}
	script, err := restoreCommand(engine)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, transferTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, getURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("скачивание копии: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("хранилище ответило %d", resp.StatusCode)
	}
	cmd := exec.CommandContext(cctx, d.Docker, "exec", "-i", dbContainer(id), "sh", "-c", script)
	cmd.Stdin = resp.Body // копия идёт потоком, на диск ноды не пишется
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("восстановление не удалось: %w: %s", err, tail(out.String(), 1500))
	}
	return nil
}
