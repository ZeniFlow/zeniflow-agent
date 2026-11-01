package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"

	agentv1 "github.com/ZeniFlow/zeniflow-agent/proto/agent/v1"
)

type fakeClient struct {
	agentv1.AgentServiceClient
	calls int
	der   []byte
}

func (f *fakeClient) Renew(context.Context, *agentv1.RenewRequest, ...grpc.CallOption) (*agentv1.RenewResponse, error) {
	f.calls++
	return &agentv1.RenewResponse{ClientCertDer: f.der}, nil
}

func certFor(t *testing.T, key *ecdsa.PrivateKey, notAfter time.Time) []byte {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "n"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter,
	}, &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "ca"}}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func TestRenewIfDue(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	dir := t.TempDir()
	fresh := certFor(t, key, time.Now().Add(100*24*time.Hour))
	st := &state{dir: dir, cert: tls.Certificate{Certificate: [][]byte{fresh}, PrivateKey: key}}
	f := &fakeClient{der: certFor(t, key, time.Now().Add(365*24*time.Hour))}

	if err := renewIfDue(context.Background(), st, f); err != nil || f.calls != 0 {
		t.Fatalf("свежий сертификат не должен продлеваться: err=%v calls=%d", err, f.calls)
	}

	st.cert.Certificate = [][]byte{certFor(t, key, time.Now().Add(10*24*time.Hour))}
	if err := renewIfDue(context.Background(), st, f); !errors.Is(err, errRenewed) || f.calls != 1 {
		t.Fatalf("ждали продление: err=%v calls=%d", err, f.calls)
	}
	if string(st.cert.Certificate[0]) != string(f.der) {
		t.Error("в памяти остался старый сертификат")
	}
	if _, err := os.Stat(filepath.Join(dir, certFile)); err != nil {
		t.Errorf("новый сертификат не записан: %v", err)
	}
}
