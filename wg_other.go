//go:build !linux

package main

import (
	"log/slog"

	agentv1 "github.com/ZeniFlow/zeniflow-agent/proto/agent/v1"
)

// applyOverlay: WireGuard настраиваем только на Linux, на других системах (разработка) конфигурация лишь принимается.
func applyOverlay(_ *state, cfg *agentv1.OverlayConfig) error {
	slog.Info("overlay: применение доступно только на Linux", "peers", len(cfg.GetPeers()))
	return nil
}
