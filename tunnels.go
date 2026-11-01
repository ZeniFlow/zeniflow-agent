package main

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/netip"
	"sync"

	"github.com/ZeniFlow/zeniflow-agent/tunnel"
)

// allowedTargetCIDR — единственная сеть, в которую туннель может открывать соединения. Туннельный сервер публичный,
// поэтому нода не должна пускать через него никуда, кроме VM в overlay. Значение можно изменить только при сборке
// (-ldflags "-X main.allowedTargetCIDR=…"): для стенда разработки без overlay. Во время работы его не поменять.
var allowedTargetCIDR = "10.99.0.0/16"

var overlayPrefix = netip.MustParsePrefix(allowedTargetCIDR)

func allowOverlayTarget(target string) bool {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && overlayPrefix.Contains(ip)
}

// tunnelManager держит по клиенту туннеля на каждый сервер из списка панели и убирает лишние.
type tunnelManager struct {
	ctx context.Context
	st  *state

	mu      sync.Mutex
	clients map[string]context.CancelFunc
}

func newTunnelManager(ctx context.Context, st *state) *tunnelManager {
	return &tunnelManager{ctx: ctx, st: st, clients: map[string]context.CancelFunc{}}
}

func (m *tunnelManager) set(servers []string) {
	want := map[string]bool{}
	for _, s := range servers {
		want[s] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for addr, cancel := range m.clients {
		if !want[addr] {
			cancel()
			delete(m.clients, addr)
			slog.Info("туннель закрыт по просьбе панели", "server", addr)
		}
	}
	for addr := range want {
		if _, ok := m.clients[addr]; ok {
			continue
		}
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			slog.Warn("неверный адрес туннеля", "server", addr)
			continue
		}
		ctx, cancel := context.WithCancel(m.ctx)
		m.clients[addr] = cancel
		c := &tunnel.Client{
			Server: addr,
			TLS: &tls.Config{
				MinVersion: tls.VersionTLS13, RootCAs: m.st.caPool, ServerName: host,
				// Сертификат берётся при каждом подключении: после продления используется новый.
				GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &m.st.cert, nil },
			},
			Allow: allowOverlayTarget,
		}
		go c.Run(ctx)
		slog.Info("туннель запущен", "server", addr)
	}
}
