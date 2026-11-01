package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentv1 "github.com/ZeniFlow/zeniflow-agent/proto/agent/v1"
)

func statusCmd(action string) *agentv1.Command {
	return &agentv1.Command{Id: "c1", Payload: &agentv1.Command_VmAction{VmAction: &agentv1.VMAction{Vmid: 101, Action: action}}}
}

func TestExecute(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	if res := execute(ctx, dir, statusCmd("status")); res.Ok || !strings.Contains(res.Error, "не настроен") || res.Id != "c1" {
		t.Errorf("без настроек: %+v", res)
	}

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"status": "running"}})
	}))
	defer srv.Close()
	cfg, _ := json.Marshal(proxmoxConfig{URL: srv.URL, Node: "pve", Token: "root@pam!zf=s"})
	if err := os.WriteFile(filepath.Join(dir, proxmoxFile), cfg, 0o600); err != nil {
		t.Fatal(err)
	}

	res := execute(ctx, dir, statusCmd("status"))
	if !res.Ok || res.VmStatus != "running" || res.Vmid != 101 || gotAuth != "PVEAPIToken=root@pam!zf=s" {
		t.Errorf("status: %+v auth=%q", res, gotAuth)
	}
	if res := execute(ctx, dir, statusCmd("format")); res.Ok || res.Error == "" {
		t.Errorf("неизвестное действие должно давать ошибку: %+v", res)
	}
	bad := &agentv1.Command{Id: "c2", Payload: &agentv1.Command_CreateVm{CreateVm: &agentv1.CreateVM{Name: "x"}}}
	if res := execute(ctx, dir, bad); res.Ok || res.Id != "c2" {
		t.Errorf("create без шаблона и ресурсов: %+v", res)
	}
}

func TestExecuteSiteCommandsRejectBadInput(t *testing.T) {
	ctx := context.Background()
	// Команды сайтов не зависят от настроек Proxmox, но проверяют вход до запуска чего-либо.
	bad := &agentv1.Command{Id: "s1", Payload: &agentv1.Command_DeploySite{DeploySite: &agentv1.DeploySite{
		SiteId: "s1", DeployId: "d1", RepoUrl: "file:///etc", Branch: "main", CpuMilli: 500, MemoryMb: 256, Pids: 64, HostPort: 20000, BindIp: "10.99.0.1"}}}
	if res := execute(ctx, t.TempDir(), bad); res.Ok || res.Id != "s1" || res.Error == "" {
		t.Errorf("репозиторий с диска ноды должен отклоняться: %+v", res)
	}
	unknown := &agentv1.Command{Id: "s2", Payload: &agentv1.Command_SiteAction{SiteAction: &agentv1.SiteAction{SiteId: "s1", Action: "format"}}}
	if res := execute(ctx, t.TempDir(), unknown); res.Ok || res.Error == "" {
		t.Errorf("неизвестное действие: %+v", res)
	}
	badID := &agentv1.Command{Id: "s3", Payload: &agentv1.Command_SiteAction{SiteAction: &agentv1.SiteAction{SiteId: "../x", Action: "stop"}}}
	if res := execute(ctx, t.TempDir(), badID); res.Ok {
		t.Errorf("неверный идентификатор сайта: %+v", res)
	}
}

func TestExecuteDBCommandsRejectBadInput(t *testing.T) {
	ctx := context.Background()
	bad := &agentv1.Command{Id: "d1", Payload: &agentv1.Command_DeployDb{DeployDb: &agentv1.DeployDB{
		DbId: "d1", Engine: "postgres", Version: "latest", DbName: "app", User: "app", Password: "abcdefghij0123456789", CpuMilli: 500, MemoryMb: 256, Pids: 64, HostPort: 30000, BindIp: "10.99.0.1"}}}
	if res := execute(ctx, t.TempDir(), bad); res.Ok || res.Id != "d1" || res.Error == "" {
		t.Errorf("произвольная версия образа должна отклоняться: %+v", res)
	}
	unknown := &agentv1.Command{Id: "d2", Payload: &agentv1.Command_DbAction{DbAction: &agentv1.DBAction{DbId: "d1", Action: "format"}}}
	if res := execute(ctx, t.TempDir(), unknown); res.Ok || res.Error == "" {
		t.Errorf("неизвестное действие: %+v", res)
	}
	badID := &agentv1.Command{Id: "d3", Payload: &agentv1.Command_DbAction{DbAction: &agentv1.DBAction{DbId: "../x", Action: "remove"}}}
	if res := execute(ctx, t.TempDir(), badID); res.Ok {
		t.Errorf("неверный идентификатор: %+v", res)
	}
}
