package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

// testCA создаёт CA и сервер, как это делает панель: серверный сертификат подписан CA, цепочка содержит и CA.
func testServer(t *testing.T, hosts ...string) (addr, caFingerprint string) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "server"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			leafTmpl.IPAddresses = append(leafTmpl.IPAddresses, ip)
		} else {
			leafTmpl.DNSNames = append(leafTmpl.DNSNames, h)
		}
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER, caDER}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS13,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _ = conn.(*tls.Conn).Handshake(); conn.Close() }()
		}
	}()
	sum := sha256.Sum256(caDER)
	return ln.Addr().String(), hex.EncodeToString(sum[:])
}

func handshake(t *testing.T, addr string, cfg *tls.Config) error {
	t.Helper()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", addr, cfg)
	if err != nil {
		return err
	}
	return conn.Close()
}

func TestPinnedTLS(t *testing.T) {
	addr, fp := testServer(t, "127.0.0.1", "localhost")

	if err := handshake(t, addr, pinnedTLS("127.0.0.1", fp)); err != nil {
		t.Errorf("верный отпечаток должен приниматься: %v", err)
	}
	if err := handshake(t, addr, pinnedTLS("localhost", strings.ToUpper(fp))); err != nil {
		t.Errorf("отпечаток в верхнем регистре тоже должен приниматься: %v", err)
	}
	if err := handshake(t, addr, pinnedTLS("127.0.0.1", strings.Repeat("00", 32))); err == nil {
		t.Error("чужой отпечаток принят: подмена панели прошла бы незамеченной")
	}
	if err := handshake(t, addr, pinnedTLS("evil.example", fp)); err == nil {
		t.Error("сертификат принят для чужого имени хоста")
	}
}
