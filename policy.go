package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"google.golang.org/protobuf/encoding/protojson"

	agentv1 "github.com/ZeniFlow/zeniflow-agent/proto/agent/v1"
)

const policyFile = "policy.json"

// savePolicy сохраняет политику на диск: после перезагрузки ноды правила нужно поставить сразу, не дожидаясь панели.
func savePolicy(dir string, p *agentv1.NetworkPolicy) error {
	raw, err := protojson.Marshal(p)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, policyFile+".new")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, policyFile))
}

func loadPolicy(dir string) *agentv1.NetworkPolicy {
	raw, err := os.ReadFile(filepath.Join(dir, policyFile))
	if err != nil {
		return nil
	}
	p := &agentv1.NetworkPolicy{}
	if err := protojson.Unmarshal(raw, p); err != nil {
		slog.Warn("сохранённая политика не читается", "err", err)
		return nil
	}
	return p
}

// onPolicy принимает политику от панели: запоминает, сохраняет и применяет правила, если они изменились.
func onPolicy(ctx context.Context, dir string, p *agentv1.NetworkPolicy) {
	if old := currentPolicy.Load(); old != nil && old.String() == p.String() {
		return
	}
	currentPolicy.Store(p)
	if err := savePolicy(dir, p); err != nil {
		slog.Warn("политика не сохранена", "err", err)
	}
	applyFirewall(ctx)
}
