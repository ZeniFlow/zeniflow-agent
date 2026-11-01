// Package proxmox — клиент API Proxmox VE. Агент работает на самой ноде и ходит в API по локальному адресу.
package proxmox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client обращается к API Proxmox по токену (PVEAPIToken). Токен создаётся на ноде и хранится только в состоянии агента.
type Client struct {
	base  string // например https://127.0.0.1:8006
	token string // user@realm!id=secret
	http  *http.Client
	// PollEvery — период опроса задач. ponytail: фиксированный опрос, хватит для создания VM; WebSocket/события не нужны.
	PollEvery time.Duration
}

// New создаёт клиент. httpc нужен вызывающему: у Proxmox по умолчанию самоподписанный сертификат.
func New(base, token string, httpc *http.Client) *Client {
	return &Client{base: strings.TrimRight(base, "/"), token: token, http: httpc, PollEvery: 2 * time.Second}
}

// APIError — ответ Proxmox с ошибкой.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("proxmox: HTTP %d: %s", e.Status, e.Body) }

func (c *Client) do(ctx context.Context, method, path string, form url.Values, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/api2/json"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+c.token)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	if out == nil {
		return nil
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("proxmox: ответ не JSON: %w", err)
	}
	return json.Unmarshal(env.Data, out)
}

// waitTask ждёт завершения задачи (UPID) и возвращает ошибку, если она закончилась не с кодом OK.
func (c *Client) waitTask(ctx context.Context, node, upid string) error {
	for {
		var st struct {
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		}
		if err := c.do(ctx, "GET", "/nodes/"+node+"/tasks/"+url.PathEscape(upid)+"/status", nil, &st); err != nil {
			return err
		}
		if st.Status == "stopped" {
			if st.ExitStatus != "OK" {
				return fmt.Errorf("proxmox: задача завершилась с ошибкой: %s", st.ExitStatus)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.PollEvery):
		}
	}
}

// post отправляет изменяющий запрос, который возвращает UPID, и ждёт его выполнения.
func (c *Client) post(ctx context.Context, method, path, node string, form url.Values) error {
	var upid string
	if err := c.do(ctx, method, path, form, &upid); err != nil {
		return err
	}
	return c.waitTask(ctx, node, upid)
}

// Spec — параметры новой VM. Шаблон (Ubuntu 24.04 с cloud-init) подготовлен на ноде заранее.
type Spec struct {
	TemplateID int
	Name       string
	Cores      int
	MemoryMB   int
	DiskGB     int // итоговый размер диска, не меньше диска шаблона
	SSHKeys    string
	Password   string // пароль пользователя root, сгенерирован панелью
	// OverlayCIDR — адрес VM в overlay с маской (10.99.0.5/16). Подключается вторым сетевым интерфейсом на мосту Bridge,
	// основной интерфейс остаётся на DHCP и даёт выход в интернет. Нода отвечает на ARP за остальные адреса overlay (proxy ARP).
	OverlayCIDR string
	Bridge      string
}

func (s Spec) validate() error {
	switch {
	case s.TemplateID <= 0:
		return errors.New("не указан шаблон")
	case s.Name == "" || strings.ContainsAny(s.Name, " /\\"):
		return errors.New("неверное имя VM")
	case s.Cores < 1 || s.MemoryMB < 128 || s.DiskGB < 1:
		return errors.New("неверные ресурсы VM")
	}
	if s.OverlayCIDR != "" {
		if _, _, err := net.ParseCIDR(s.OverlayCIDR); err != nil || s.Bridge == "" || strings.ContainsAny(s.Bridge, ", =") {
			return errors.New("неверные параметры overlay")
		}
	}
	return nil
}

// CreateVM клонирует шаблон, задаёт ресурсы и cloud-init и возвращает номер новой VM. VM остаётся остановленной.
// Если после клонирования что-то не вышло, клон удаляется: нода не засоряется недособранными VM.
func (c *Client) CreateVM(ctx context.Context, node string, s Spec) (_ int, err error) {
	if err := s.validate(); err != nil {
		return 0, err
	}
	var next string
	if err := c.do(ctx, "GET", "/cluster/nextid", nil, &next); err != nil {
		return 0, err
	}
	vmid, err := strconv.Atoi(next)
	if err != nil {
		return 0, fmt.Errorf("proxmox: неверный nextid %q", next)
	}
	vm := fmt.Sprintf("/nodes/%s/qemu/%d", node, vmid)

	err = c.post(ctx, "POST", fmt.Sprintf("/nodes/%s/qemu/%d/clone", node, s.TemplateID), node, url.Values{
		"newid": {next}, "name": {s.Name}, "full": {"1"},
	})
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			// Отдельный контекст: исходный мог быть отменён, а убрать за собой нужно в любом случае.
			cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_ = c.Delete(cctx, node, vmid)
		}
	}()

	cfg := url.Values{
		"cores": {strconv.Itoa(s.Cores)}, "memory": {strconv.Itoa(s.MemoryMB)},
		"ciuser": {"root"}, "ipconfig0": {"ip=dhcp"},
	}
	if s.OverlayCIDR != "" {
		cfg.Set("net1", "virtio,bridge="+s.Bridge)
		cfg.Set("ipconfig1", "ip="+s.OverlayCIDR)
	}
	if s.Password != "" {
		cfg.Set("cipassword", s.Password)
	}
	if s.SSHKeys != "" {
		// Proxmox требует, чтобы ключи были закодированы в процентах ещё до кодирования формы.
		cfg.Set("sshkeys", strings.ReplaceAll(url.QueryEscape(s.SSHKeys), "+", "%20"))
	}
	// PUT применяет настройки синхронно (POST вернул бы задачу).
	if err = c.do(ctx, "PUT", vm+"/config", cfg, nil); err != nil {
		return 0, err
	}
	// Диск расширяется до итогового размера; если шаблон уже не меньше, Proxmox вернёт ошибку, мы её не скрываем.
	if err = c.do(ctx, "PUT", vm+"/resize", url.Values{"disk": {"scsi0"}, "size": {fmt.Sprintf("%dG", s.DiskGB)}}, nil); err != nil {
		return 0, err
	}
	return vmid, nil
}

// Action — действие над VM.
type Action string

const (
	Start    Action = "start"
	Stop     Action = "stop"     // жёсткое выключение
	Shutdown Action = "shutdown" // по сигналу ACPI
	Reboot   Action = "reboot"
)

func (c *Client) Do(ctx context.Context, node string, vmid int, a Action) error {
	switch a {
	case Start, Stop, Shutdown, Reboot:
	default:
		return fmt.Errorf("неизвестное действие %q", a)
	}
	return c.post(ctx, "POST", fmt.Sprintf("/nodes/%s/qemu/%d/status/%s", node, vmid, a), node, url.Values{})
}

// Status возвращает состояние VM: running, stopped и т. д.
func (c *Client) Status(ctx context.Context, node string, vmid int) (string, error) {
	var st struct {
		Status string `json:"status"`
	}
	err := c.do(ctx, "GET", fmt.Sprintf("/nodes/%s/qemu/%d/status/current", node, vmid), nil, &st)
	return st.Status, err
}

// Delete удаляет VM вместе с дисками. Работающую VM сначала останавливает.
func (c *Client) Delete(ctx context.Context, node string, vmid int) error {
	if st, err := c.Status(ctx, node, vmid); err == nil && st == "running" {
		if err := c.Do(ctx, node, vmid, Stop); err != nil {
			return err
		}
	}
	return c.post(ctx, "DELETE", fmt.Sprintf("/nodes/%s/qemu/%d?purge=1&destroy-unreferenced-disks=1", node, vmid), node, nil)
}
