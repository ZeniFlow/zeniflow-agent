package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"time"
)

// firewallMu не даёт двум применениям (по таймеру и по новой политике) идти одновременно.
var firewallMu sync.Mutex

func nft(ctx context.Context, rules string) error {
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = bytes.NewBufferString(rules)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft: %w: %s", err, out)
	}
	return nil
}

// allowOverlayForward разрешает пересылку внутри overlay в iptables. Если на ноде стоит docker, он выставляет у FORWARD политику
// «запретить», и пакеты между VM и нодами (маршрутизация внутри overlay) пропали бы, хотя наши правила их пропускают:
// accept в одной цепочке не отменяет drop в другой. Наши запреты стоят раньше (приоритет -10) и действуют по-прежнему.
func allowOverlayForward(ctx context.Context) {
	if _, err := exec.LookPath("iptables"); err != nil {
		return
	}
	rule := []string{"FORWARD", "-s", overlayNet, "-d", overlayNet, "-j", "ACCEPT"}
	if exec.CommandContext(ctx, "iptables", append([]string{"-w", "-C"}, rule...)...).Run() == nil {
		return
	}
	if out, err := exec.CommandContext(ctx, "iptables", "-w", "-I", "FORWARD", "1", "-s", overlayNet, "-d", overlayNet, "-j", "ACCEPT").CombinedOutput(); err != nil {
		slog.Warn("iptables: пересылка внутри overlay не разрешена", "err", err, "out", string(out))
	}
}

// applyFirewall применяет правила и запоминает результат для heartbeat. Основные правила (inet) обязательны, мостовые нужны только
// для VM Proxmox: если ядро их не поддерживает, это отмечается как сбой, чтобы владелец увидел.
func applyFirewall(ctx context.Context) {
	firewallMu.Lock()
	defer firewallMu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := nft(cctx, nftRules(currentPolicy.Load())); err != nil {
		slog.Error("фаервол не применён", "err", err)
		firewallState.Store("failed")
		return
	}
	allowOverlayForward(cctx)
	if err := nft(cctx, nftBridgeRules()); err != nil {
		slog.Warn("правила моста не применены (нет модуля nft_bridge?)", "err", err)
		firewallState.Store("failed")
		return
	}
	firewallState.Store("ok")
}

// watchFirewall применяет правила при старте и проверяет раз в пять минут: если таблицу кто-то удалил, возвращает её.
func watchFirewall(ctx context.Context) {
	applyFirewall(ctx)
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := exec.CommandContext(ctx, "nft", "list", "table", "inet", "zeniflow").Run(); err != nil {
				slog.Warn("таблица zeniflow пропала, применяем заново")
				applyFirewall(ctx)
			}
		}
	}
}
