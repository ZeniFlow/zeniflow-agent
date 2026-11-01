package tunnel

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"testing"
	"time"
)

type pki struct {
	ca   *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	ca, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &pki{ca: ca, key: key, pool: pool}
}

func (p *pki) issue(t *testing.T, cn string, server bool) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func (p *pki) clientTLS(t *testing.T, cn string) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: p.pool, Certificates: []tls.Certificate{p.issue(t, cn, false)}}
}

// startServer запускает туннельный сервер и возвращает его адрес.
func startServer(t *testing.T, p *pki) (*Server, string, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{p.issue(t, "server", true)},
		ClientCAs: p.pool, ClientAuth: tls.RequireAndVerifyClientCert,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer()
	go s.Serve(ctx, ln) //nolint:errcheck
	return s, ln.Addr().String(), ctx
}

func echoServer(t *testing.T) string {
	t.Helper()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().String()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("не дождались: %s", what)
}

func TestForwardEndToEnd(t *testing.T) {
	p := newPKI(t)
	srv, addr, ctx := startServer(t, p)
	target := echoServer(t)

	if _, err := srv.Connect(ctx, "node-1", target); err != ErrClientOffline {
		t.Fatalf("без клиента: %v", err)
	}

	cl := &Client{Server: addr, TLS: p.clientTLS(t, "node-1"), Allow: func(tg string) bool { return tg == target }}
	cctx, stop := context.WithCancel(ctx)
	defer stop()
	go cl.Run(cctx)
	waitFor(t, "клиент подключился", func() bool { srv.mu.Lock(); defer srv.mu.Unlock(); return srv.controls["node-1"] != nil })

	pub, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Forward(ctx, pub, "node-1", target)

	// Несколько соединений подряд и одновременно, данные в обе стороны, полузакрытие.
	for i := 0; i < 3; i++ {
		c, err := net.Dial("tcp", pub.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		msg := []byte("привет через туннель")
		_, _ = c.Write(msg)
		_ = c.(*net.TCPConn).CloseWrite()
		got, err := io.ReadAll(c)
		c.Close()
		if err != nil || string(got) != string(msg) {
			t.Fatalf("эхо %d: %q %v", i, got, err)
		}
	}
}

func TestClientRefusesDisallowedTarget(t *testing.T) {
	p := newPKI(t)
	srv, addr, ctx := startServer(t, p)
	target := echoServer(t)
	cl := &Client{Server: addr, TLS: p.clientTLS(t, "node-1"), Allow: func(string) bool { return false }}
	go cl.Run(ctx)
	waitFor(t, "клиент подключился", func() bool { srv.mu.Lock(); defer srv.mu.Unlock(); return srv.controls["node-1"] != nil })

	cctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if c, err := srv.Connect(cctx, "node-1", target); err == nil {
		c.Close()
		t.Fatal("клиент не должен открывать запрещённую цель")
	}
	srv.mu.Lock()
	left := len(srv.pending)
	srv.mu.Unlock()
	if left != 0 {
		t.Errorf("ожидающие открытия должны убираться: %d", left)
	}
}

func TestOtherClientCannotAnswer(t *testing.T) {
	p := newPKI(t)
	srv, addr, ctx := startServer(t, p)

	// Законный клиент node-1 подключён, но сам не отвечает на открытия: ответить пытается node-2.
	cl1 := p.clientTLS(t, "node-1")
	c1, err := tls.Dial("tcp", addr, cl1)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	_ = writeLine(c1, hello{Op: "control"})
	r1 := bufio.NewReader(c1)
	waitFor(t, "control node-1", func() bool { srv.mu.Lock(); defer srv.mu.Unlock(); return srv.controls["node-1"] != nil })

	res := make(chan net.Conn, 1)
	go func() {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		c, _ := srv.Connect(cctx, "node-1", "10.99.0.5:22")
		res <- c
	}()
	var m openMsg
	if err := readLine(r1, &m); err != nil || m.ID == "" {
		t.Fatalf("открытие не пришло: %+v %v", m, err)
	}

	evil, err := tls.Dial("tcp", addr, p.clientTLS(t, "node-2"))
	if err != nil {
		t.Fatal(err)
	}
	_ = writeLine(evil, hello{Op: "data", ID: m.ID})
	_ = evil.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := evil.Read(make([]byte, 1)); err == nil {
		t.Error("соединение чужого клиента должно закрываться")
	}
	select {
	case c := <-res:
		if c != nil {
			t.Fatal("чужой клиент не должен получить чужое открытие")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Connect завис")
	}
}

func TestClientWithoutValidCertRejected(t *testing.T) {
	p := newPKI(t)
	_, addr, _ := startServer(t, p)
	other := newPKI(t) // чужой CA
	cfg := other.clientTLS(t, "node-1")
	cfg.RootCAs = p.pool
	c, err := tls.Dial("tcp", addr, cfg)
	if err == nil {
		// В TLS 1.3 ошибка клиентского сертификата видна при первом чтении.
		_ = writeLine(c, hello{Op: "control"})
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		_, err = c.Read(make([]byte, 1))
		c.Close()
	}
	if err == nil {
		t.Fatal("сертификат чужого CA должен отклоняться")
	}
}

func TestClientReconnects(t *testing.T) {
	p := newPKI(t)
	srv, addr, ctx := startServer(t, p)
	cl := &Client{Server: addr, TLS: p.clientTLS(t, "node-1"), Allow: func(string) bool { return true }}
	go cl.Run(ctx)
	waitFor(t, "первое подключение", func() bool { srv.mu.Lock(); defer srv.mu.Unlock(); return srv.controls["node-1"] != nil })

	srv.mu.Lock()
	_ = srv.controls["node-1"].conn.Close() // обрыв со стороны сервера
	srv.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	waitFor(t, "переподключение", func() bool { srv.mu.Lock(); defer srv.mu.Unlock(); return srv.controls["node-1"] != nil })
}
