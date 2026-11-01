package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	agentv1 "github.com/ZeniFlow/zeniflow-agent/proto/agent/v1"
)

const (
	wgFile      = "wg.key"
	wgInterface = "zf0"
	wgPort      = 51820
)

// loadOrCreateWG возвращает закрытый ключ WireGuard (base64) и соответствующий публичный. Ключ создаётся один раз и не покидает ноду.
func loadOrCreateWG(dir string) (priv, pub string, err error) {
	path := filepath.Join(dir, wgFile)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		k, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			return "", "", err
		}
		priv = base64.StdEncoding.EncodeToString(k.Bytes())
		if err := os.WriteFile(path, []byte(priv+"\n"), 0o600); err != nil {
			return "", "", err
		}
		return priv, base64.StdEncoding.EncodeToString(k.PublicKey().Bytes()), nil
	}
	if err != nil {
		return "", "", err
	}
	priv = strings.TrimSpace(string(raw))
	b, err := base64.StdEncoding.DecodeString(priv)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", wgFile, err)
	}
	k, err := ecdh.X25519().NewPrivateKey(b)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", wgFile, err)
	}
	return priv, base64.StdEncoding.EncodeToString(k.PublicKey().Bytes()), nil
}

// renderWG собирает конфигурацию для `wg syncconf`. Каждому пиру разрешены только его адрес и адреса его VM (/32):
// нода не может подсунуть чужой адрес. Keepalive нужен, чтобы держать проход через NAT.
func renderWG(priv string, cfg *agentv1.OverlayConfig) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Interface]\nPrivateKey = %s\nListenPort = %d\n", priv, wgPort)
	for _, p := range cfg.GetPeers() {
		allowed := p.GetOverlayIp() + "/32"
		for _, ip := range p.GetRoutedIps() {
			allowed += ", " + ip + "/32"
		}
		fmt.Fprintf(&b, "\n[Peer]\nPublicKey = %s\nAllowedIPs = %s\nPersistentKeepalive = 25\n", p.GetPublicKey(), allowed)
		if p.GetEndpoint() != "" {
			fmt.Fprintf(&b, "Endpoint = %s\n", p.GetEndpoint())
		}
	}
	return b.String()
}
