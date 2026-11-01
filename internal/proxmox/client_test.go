package proxmox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePVE изображает Proxmox: запоминает запросы, каждую изменяющую операцию превращает в задачу, которая завершается со второго опроса.
type fakePVE struct {
	mu       sync.Mutex
	calls    []string
	forms    map[string]url.Values
	failCfg  bool
	status   string
	polls    int
	taskExit string
}

func (f *fakePVE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "PVEAPIToken=root@pam!zf=secret" {
		http.Error(w, "no auth", 401)
		return
	}
	_ = r.ParseForm()
	key := r.Method + " " + strings.TrimPrefix(r.URL.Path, "/api2/json")
	f.calls = append(f.calls, key)
	if f.forms == nil {
		f.forms = map[string]url.Values{}
	}
	f.forms[key] = r.PostForm
	reply := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	switch {
	case key == "GET /cluster/nextid":
		reply("205")
	case strings.Contains(key, "/tasks/"):
		f.polls++
		if f.polls%2 == 1 {
			reply(map[string]string{"status": "running"})
		} else {
			reply(map[string]string{"status": "stopped", "exitstatus": f.taskExit})
		}
	case strings.HasSuffix(key, "/status/current"):
		reply(map[string]string{"status": f.status})
	case key == "PUT /nodes/pve/qemu/205/config" && f.failCfg:
		http.Error(w, "boom", 500)
	case strings.HasSuffix(key, "/config"), strings.HasSuffix(key, "/resize"):
		reply(nil)
	default: // clone, status/*, DELETE
		reply("UPID:pve:1")
	}
}

func newTest(t *testing.T) (*Client, *fakePVE) {
	f := &fakePVE{status: "stopped", taskExit: "OK"}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := New(srv.URL, "root@pam!zf=secret", srv.Client())
	c.PollEvery = time.Millisecond
	return c, f
}

var spec = Spec{TemplateID: 9000, Name: "web-prod", Cores: 2, MemoryMB: 2048, DiskGB: 40, SSHKeys: "ssh-ed25519 AAA key one", Password: "p@ss word"}

func TestCreateVM(t *testing.T) {
	c, f := newTest(t)
	vmid, err := c.CreateVM(context.Background(), "pve", spec)
	if err != nil || vmid != 205 {
		t.Fatalf("vmid=%d err=%v", vmid, err)
	}
	clone := f.forms["POST /nodes/pve/qemu/9000/clone"]
	if clone.Get("newid") != "205" || clone.Get("name") != "web-prod" || clone.Get("full") != "1" {
		t.Errorf("клонирование: %v", clone)
	}
	cfg := f.forms["PUT /nodes/pve/qemu/205/config"]
	if cfg.Get("cores") != "2" || cfg.Get("memory") != "2048" || cfg.Get("cipassword") != "p@ss word" || cfg.Get("ciuser") != "root" {
		t.Errorf("настройки: %v", cfg)
	}
	// Ключи кодируются дважды: после разбора формы остаются проценты, а пробелы — %20.
	if got := cfg.Get("sshkeys"); got != "ssh-ed25519%20AAA%20key%20one" {
		t.Errorf("sshkeys = %q", got)
	}
	if cfg.Has("net1") || cfg.Has("ipconfig1") {
		t.Errorf("без overlay второй интерфейс не нужен: %v", cfg)
	}
	if f.forms["PUT /nodes/pve/qemu/205/resize"].Get("size") != "40G" {
		t.Error("диск не расширен")
	}
}

func TestCreateVMCleansUpOnFailure(t *testing.T) {
	c, f := newTest(t)
	f.failCfg = true
	if _, err := c.CreateVM(context.Background(), "pve", spec); err == nil {
		t.Fatal("ждали ошибку")
	}
	if !contains(f.calls, "DELETE /nodes/pve/qemu/205") {
		t.Errorf("клон не удалён: %v", f.calls)
	}
}

func TestTaskFailureAndValidation(t *testing.T) {
	c, f := newTest(t)
	f.taskExit = "clone failed"
	if _, err := c.CreateVM(context.Background(), "pve", spec); err == nil || !strings.Contains(err.Error(), "clone failed") {
		t.Errorf("ошибка задачи потерялась: %v", err)
	}
	bad := spec
	bad.Name = "has space"
	if _, err := c.CreateVM(context.Background(), "pve", bad); err == nil {
		t.Error("имя с пробелом должно отклоняться")
	}
	if err := c.Do(context.Background(), "pve", 1, "format"); err == nil {
		t.Error("неизвестное действие должно отклоняться")
	}
}

func TestActionsAndDelete(t *testing.T) {
	c, f := newTest(t)
	ctx := context.Background()
	if err := c.Do(ctx, "pve", 205, Reboot); err != nil || !contains(f.calls, "POST /nodes/pve/qemu/205/status/reboot") {
		t.Fatalf("reboot: %v %v", err, f.calls)
	}
	f.status = "running"
	f.calls = nil
	if err := c.Delete(ctx, "pve", 205); err != nil {
		t.Fatal(err)
	}
	stop, del := index(f.calls, "POST /nodes/pve/qemu/205/status/stop"), index(f.calls, "DELETE /nodes/pve/qemu/205")
	if stop < 0 || del < stop {
		t.Errorf("работающую VM нужно сначала остановить: %v", f.calls)
	}
	wrong := New(strings.Replace("http://x", "x", "127.0.0.1:1", 1), "t", http.DefaultClient)
	if _, err := wrong.Status(ctx, "pve", 1); err == nil {
		t.Error("ждали ошибку сети")
	}
}

func contains(l []string, s string) bool { return index(l, s) >= 0 }
func index(l []string, s string) int {
	for i, v := range l {
		if v == s {
			return i
		}
	}
	return -1
}

func TestCreateVMWithOverlay(t *testing.T) {
	c, f := newTest(t)
	s := spec
	s.OverlayCIDR, s.Bridge = "10.99.0.5/16", "vmbr0"
	if _, err := c.CreateVM(context.Background(), "pve", s); err != nil {
		t.Fatal(err)
	}
	cfg := f.forms["PUT /nodes/pve/qemu/205/config"]
	if cfg.Get("net1") != "virtio,bridge=vmbr0" || cfg.Get("ipconfig1") != "ip=10.99.0.5/16" || cfg.Get("ipconfig0") != "ip=dhcp" {
		t.Errorf("overlay-интерфейс: %v", cfg)
	}
	for _, bad := range []Spec{{OverlayCIDR: "10.99.0.5", Bridge: "vmbr0"}, {OverlayCIDR: "10.99.0.5/16"}, {OverlayCIDR: "10.99.0.5/16", Bridge: "vmbr0,firewall=0"}} {
		b := spec
		b.OverlayCIDR, b.Bridge = bad.OverlayCIDR, bad.Bridge
		if _, err := c.CreateVM(context.Background(), "pve", b); err == nil {
			t.Errorf("параметры %+v должны отклоняться (иначе можно подставить чужие опции сети)", bad)
		}
	}
}
