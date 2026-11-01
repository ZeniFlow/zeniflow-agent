package main

import (
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ZeniFlow/zeniflow-agent/internal/docker"
	"github.com/ZeniFlow/zeniflow-agent/internal/proxmox"
	agentv1 "github.com/ZeniFlow/zeniflow-agent/proto/agent/v1"
)

const proxmoxFile = "proxmox.json"

// proxmoxConfig — доступ к API Proxmox на этой ноде. Токен выдаётся на самом Proxmox и не покидает ноду.
type proxmoxConfig struct {
	URL   string `json:"url"`  // обычно https://127.0.0.1:8006
	Node  string `json:"node"` // имя ноды внутри Proxmox
	Token string `json:"token"`
	// Шаблон по умолчанию (Ubuntu 24.04 с cloud-init). Используется, если панель не указала свой.
	TemplateID int `json:"template_id"`
	// Мост Proxmox, к которому подключаются VM. Нужен для overlay: по умолчанию vmbr0.
	Bridge string `json:"bridge"`
}

func loadProxmox(dir string) (*proxmox.Client, proxmoxConfig, error) {
	raw, err := os.ReadFile(filepath.Join(dir, proxmoxFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, proxmoxConfig{}, errors.New("Proxmox на ноде не настроен: выполните zeniflow-agent proxmox")
	}
	if err != nil {
		return nil, proxmoxConfig{}, err
	}
	var c proxmoxConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, proxmoxConfig{}, fmt.Errorf("%s: %w", proxmoxFile, err)
	}
	u, err := url.Parse(c.URL)
	if err != nil || c.Node == "" || c.Token == "" {
		return nil, proxmoxConfig{}, fmt.Errorf("%s: нужны url, node и token", proxmoxFile)
	}
	// У Proxmox самоподписанный сертификат: проверку отключаем только для адреса самой ноды.
	tr := &http.Transport{}
	if h := u.Hostname(); h == "127.0.0.1" || h == "localhost" || h == "::1" {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	return proxmox.New(c.URL, c.Token, &http.Client{Transport: tr, Timeout: 30 * time.Second}), c, nil
}

func cmdProxmox(args []string) error {
	fs := flag.NewFlagSet("proxmox", flag.ExitOnError)
	dir := fs.String("state-dir", defaultStateDir(), "каталог состояния")
	cfg := proxmoxConfig{}
	fs.StringVar(&cfg.URL, "url", "https://127.0.0.1:8006", "адрес API Proxmox")
	fs.StringVar(&cfg.Node, "node", "", "имя ноды в Proxmox")
	fs.StringVar(&cfg.Token, "token", "", "токен API: user@realm!id=secret")
	fs.IntVar(&cfg.TemplateID, "template", 0, "номер шаблона VM по умолчанию")
	fs.StringVar(&cfg.Bridge, "bridge", "vmbr0", "мост Proxmox для VM")
	_ = fs.Parse(args)
	if cfg.Node == "" || cfg.Token == "" {
		return errors.New("нужны --node и --token")
	}
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(*dir, proxmoxFile), raw, 0o600); err != nil {
		return err
	}
	fmt.Println("Настройки Proxmox сохранены. Перезапускать агента не нужно.")
	return nil
}

// execute выполняет команду панели. Результат всегда возвращается панели, даже при ошибке.
func execute(ctx context.Context, dir string, cmd *agentv1.Command) *agentv1.CommandResult {
	res := &agentv1.CommandResult{Id: cmd.GetId()}
	switch p := cmd.GetPayload().(type) {
	case *agentv1.Command_DeploySite:
		return executeDeploy(ctx, res, p.DeploySite)
	case *agentv1.Command_SiteAction:
		return executeSiteAction(ctx, res, p.SiteAction)
	case *agentv1.Command_DeployDb:
		return executeDeployDB(ctx, res, p.DeployDb)
	case *agentv1.Command_DbAction:
		return executeDBAction(ctx, res, p.DbAction)
	}
	pve, cfg, err := loadProxmox(dir)
	node := cfg.Node
	if err != nil {
		res.Error = err.Error()
		return res
	}
	switch p := cmd.GetPayload().(type) {
	case *agentv1.Command_CreateVm:
		c := p.CreateVm
		tmpl := int(c.GetTemplateId())
		if tmpl == 0 {
			tmpl = cfg.TemplateID
		}
		vmid, err := pve.CreateVM(ctx, node, proxmox.Spec{
			TemplateID: tmpl, Name: c.GetName(), Cores: int(c.GetCores()), MemoryMB: int(c.GetMemoryMb()),
			DiskGB: int(c.GetDiskGb()), SSHKeys: c.GetSshKeys(), Password: c.GetPassword(),
			OverlayCIDR: c.GetOverlayCidr(), Bridge: cmp.Or(cfg.Bridge, "vmbr0"),
		})
		res.Ok, res.Vmid = err == nil, int32(vmid)
		if err != nil {
			res.Error = err.Error()
		}
	case *agentv1.Command_VmAction:
		a := p.VmAction
		vmid := int(a.GetVmid())
		switch a.GetAction() {
		case "status":
			res.VmStatus, err = pve.Status(ctx, node, vmid)
		case "delete":
			err = pve.Delete(ctx, node, vmid)
		default:
			err = pve.Do(ctx, node, vmid, proxmox.Action(a.GetAction()))
		}
		res.Ok, res.Vmid = err == nil, a.GetVmid()
		if err != nil {
			res.Error = err.Error()
		}
	default:
		res.Error = "неизвестная команда"
	}
	return res
}

// siteDriver — драйвер docker; переменная нужна, чтобы тесты подменяли его.
var siteDriver = docker.New()

// devLocal включается только при сборке для стенда разработки (-ldflags "-X main.devLocal=true"): разрешает хранилище копий
// по http и репозитории не по https. В обычной сборке и во время работы переключить это нельзя.
var devLocal = ""

func init() { siteDriver.AllowLocal = devLocal == "true" }

func executeDeploy(ctx context.Context, res *agentv1.CommandResult, d *agentv1.DeploySite) *agentv1.CommandResult {
	out, err := siteDriver.Deploy(ctx, docker.DeploySpec{
		SiteID: d.GetSiteId(), DeployID: d.GetDeployId(), RepoURL: d.GetRepoUrl(), Branch: d.GetBranch(), Env: d.GetEnv(),
		CPUMilli: int(d.GetCpuMilli()), MemoryMB: int(d.GetMemoryMb()), Pids: int(d.GetPids()),
		ContainerPort: int(d.GetContainerPort()), HostPort: int(d.GetHostPort()), BindIP: d.GetBindIp(), ImageTag: d.GetImageTag(),
	})
	res.Output, res.Stack, res.ImageTag, res.Commit, res.ContainerPort = out.Log, out.Stack, out.ImageTag, out.Commit, int32(out.ContainerPort)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Ok = true
	return res
}

func executeSiteAction(ctx context.Context, res *agentv1.CommandResult, a *agentv1.SiteAction) *agentv1.CommandResult {
	var err error
	switch a.GetAction() {
	case "stop":
		err = siteDriver.Stop(ctx, a.GetSiteId())
	case "start":
		err = siteDriver.Start(ctx, a.GetSiteId())
	case "remove":
		err = siteDriver.Remove(ctx, a.GetSiteId())
	case "logs":
		res.Output, err = siteDriver.Logs(ctx, a.GetSiteId(), int(a.GetLines()))
	case "status":
		res.VmStatus = siteDriver.Status(ctx, a.GetSiteId())
	case "images":
		res.Output = strings.Join(siteDriver.Images(ctx, a.GetSiteId()), " ")
	default:
		err = errors.New("неизвестное действие")
	}
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Ok = true
	return res
}

func executeDeployDB(ctx context.Context, res *agentv1.CommandResult, d *agentv1.DeployDB) *agentv1.CommandResult {
	err := siteDriver.CreateDB(ctx, docker.DBSpec{
		DBID: d.GetDbId(), Engine: d.GetEngine(), Version: d.GetVersion(), DBName: d.GetDbName(), User: d.GetUser(), Password: d.GetPassword(),
		CPUMilli: int(d.GetCpuMilli()), MemoryMB: int(d.GetMemoryMb()), Pids: int(d.GetPids()), HostPort: int(d.GetHostPort()), BindIP: d.GetBindIp(),
	})
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Ok = true
	return res
}

func executeDBAction(ctx context.Context, res *agentv1.CommandResult, a *agentv1.DBAction) *agentv1.CommandResult {
	var err error
	switch a.GetAction() {
	case "stop":
		err = siteDriver.StopDB(ctx, a.GetDbId())
	case "start":
		err = siteDriver.StartDB(ctx, a.GetDbId())
	case "remove":
		err = siteDriver.RemoveDB(ctx, a.GetDbId())
	case "logs":
		res.Output, err = siteDriver.DBLogs(ctx, a.GetDbId(), int(a.GetLines()))
	case "status":
		res.VmStatus = siteDriver.DBStatus(ctx, a.GetDbId())
	case "dump":
		res.Size, err = siteDriver.DumpDB(ctx, a.GetDbId(), a.GetEngine(), a.GetUrl())
	case "restore":
		err = siteDriver.RestoreDB(ctx, a.GetDbId(), a.GetEngine(), a.GetUrl())
	case "usage":
		var mb int
		mb, err = siteDriver.DBUsageMB(ctx, a.GetDbId(), a.GetEngine(), a.GetVersion())
		res.Output = strconv.Itoa(mb)
	default:
		err = errors.New("неизвестное действие")
	}
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Ok = true
	return res
}
