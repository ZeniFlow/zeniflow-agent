package main

import (
	"strings"
	"testing"

	agentv1 "github.com/ZeniFlow/zeniflow-agent/proto/agent/v1"
)

func TestWGKeyAndRender(t *testing.T) {
	dir := t.TempDir()
	priv, pub, err := loadOrCreateWG(dir)
	if err != nil || priv == "" || pub == "" || priv == pub {
		t.Fatalf("ключ не создан: %v", err)
	}
	priv2, pub2, err := loadOrCreateWG(dir)
	if err != nil || priv2 != priv || pub2 != pub {
		t.Fatal("ключ должен сохраняться между запусками")
	}

	out := renderWG(priv, &agentv1.OverlayConfig{Peers: []*agentv1.OverlayPeer{
		{PublicKey: "AAA", OverlayIp: "10.99.0.2", Endpoint: "203.0.113.5:51820", RoutedIps: []string{"10.99.0.9", "10.99.0.10"}},
		{PublicKey: "BBB", OverlayIp: "10.99.0.3"},
	}})
	for _, want := range []string{"PrivateKey = " + priv, "AllowedIPs = 10.99.0.2/32, 10.99.0.9/32, 10.99.0.10/32", "Endpoint = 203.0.113.5:51820", "AllowedIPs = 10.99.0.3/32"} {
		if !strings.Contains(out, want) {
			t.Errorf("в конфигурации нет %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "Endpoint") != 1 {
		t.Error("пир без адреса не должен получать Endpoint")
	}
}

func TestAllowOverlayTarget(t *testing.T) {
	for target, want := range map[string]bool{
		"10.99.0.7:22": true, "10.99.255.1:5432": true,
		"10.98.0.7:22": false, "192.168.1.1:22": false, "127.0.0.1:8006": false, "10.99.0.7": false, "evil.example.com:22": false,
	} {
		if got := allowOverlayTarget(target); got != want {
			t.Errorf("%s: %v", target, got)
		}
	}
}
