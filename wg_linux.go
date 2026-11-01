package main

import (
	"cmp"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	agentv1 "github.com/ZeniFlow/zeniflow-agent/proto/agent/v1"
)

func run(name string, args ...string) error {
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, out)
	}
	return nil
}

// applyOverlay поднимает интерфейс WireGuard и приводит пиров к конфигурации панели. Нужны wireguard-tools и iproute2.
func applyOverlay(st *state, cfg *agentv1.OverlayConfig) error {
	if exec.Command("ip", "link", "show", wgInterface).Run() != nil {
		if err := run("ip", "link", "add", wgInterface, "type", "wireguard"); err != nil {
			return err
		}
	}
	conf := filepath.Join(st.dir, "wg.conf")
	if err := os.WriteFile(conf, []byte(renderWG(st.wgPriv, cfg)), 0o600); err != nil {
		return err
	}
	if err := run("wg", "syncconf", wgInterface, conf); err != nil {
		return err
	}
	if err := run("ip", "address", "replace", cfg.GetAddress(), "dev", wgInterface); err != nil {
		return err
	}
	if err := run("ip", "link", "set", wgInterface, "up"); err != nil {
		return err
	}
	return routeLocalVMs(st, cfg)
}

func sysctl(path, value string) error {
	return os.WriteFile("/proc/sys/net/ipv4/"+path, []byte(value), 0o644)
}

// routeLocalVMs связывает VM на этой ноде с overlay: включает пересылку пакетов и proxy ARP на мосту (VM спрашивает
// MAC адресов overlay, нода отвечает за них) и ведёт на мост маршруты /32 к каждой VM. Без настроенного Proxmox делать нечего.
func routeLocalVMs(st *state, cfg *agentv1.OverlayConfig) error {
	_, pcfg, err := loadProxmox(st.dir)
	if err != nil {
		return nil // нода без Proxmox: у неё нет VM
	}
	bridge := cmp.Or(pcfg.Bridge, "vmbr0")
	if !bridgeRe.MatchString(bridge) {
		return fmt.Errorf("неверное имя моста %q", bridge)
	}
	for path, v := range map[string]string{
		"ip_forward":                         "1",
		"conf/" + bridge + "/proxy_arp":      "1",
		"conf/" + bridge + "/rp_filter":      "0",
		"conf/" + wgInterface + "/rp_filter": "0",
		// VM не видят друг друга на втором уровне (правило моста в nftables), а на ARP за адрес соседа отвечает сама нода
		// (proxy_arp_pvlan), так что весь трафик между VM проходит через маршрутизацию и правила политики.
		"conf/" + bridge + "/proxy_arp_pvlan": "1",
		"conf/" + bridge + "/send_redirects":  "0",
		// Запрос ARP от ноды к VM должен идти с адреса из сети overlay, иначе VM (с проверкой обратного пути) на него не ответит.
		"conf/" + bridge + "/arp_announce": "2",
	} {
		if err := sysctl(path, v); err != nil {
			return err
		}
	}
	// Адрес ноды на мосту без маршрута (маршрут overlay остаётся на интерфейсе WireGuard): его ноде нужно подставлять в ARP.
	if cfg.GetAddress() != "" {
		if err := run("ip", "address", "replace", cfg.GetAddress(), "dev", bridge, "noprefixroute"); err != nil {
			return err
		}
	}
	want := map[string]bool{}
	for _, ip := range cfg.GetLocalIps() {
		want[ip] = true
		if err := run("ip", "route", "replace", ip+"/32", "dev", bridge); err != nil {
			return err
		}
	}
	out, err := exec.Command("ip", "-o", "-4", "route", "show", "dev", bridge).Output()
	if err != nil {
		return err
	}
	for _, ip := range staleRoutes(string(out), want) {
		if err := run("ip", "route", "del", ip+"/32", "dev", bridge); err != nil {
			return err
		}
	}
	return nil
}
