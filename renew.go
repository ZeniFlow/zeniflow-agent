package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	agentv1 "github.com/ZeniFlow/zeniflow-agent/proto/agent/v1"
)

// renewBefore — за сколько до истечения агент просит новый сертификат. Запас большой: панель может быть недоступна неделями.
const renewBefore = 30 * 24 * time.Hour

var errRenewed = errors.New("сертификат продлён, переподключаемся")

func certExpiry(c tls.Certificate) (time.Time, error) {
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return time.Time{}, err
	}
	return leaf.NotAfter, nil
}

// renewIfDue продлевает сертификат, если до конца срока осталось меньше renewBefore. Ключ остаётся прежним,
// поэтому достаточно атомарно заменить один файл: обрыв посередине не оставит несовпадающую пару ключ/сертификат.
// После продления возвращает errRenewed: соединение нужно открыть заново с новым сертификатом.
func renewIfDue(ctx context.Context, st *state, client agentv1.AgentServiceClient) error {
	exp, err := certExpiry(st.cert)
	if err != nil {
		return err
	}
	if time.Until(exp) > renewBefore {
		return nil
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "zeniflow-agent"}}, st.cert.PrivateKey)
	if err != nil {
		return err
	}
	resp, err := client.Renew(ctx, &agentv1.RenewRequest{CsrDer: csr})
	if err != nil {
		return fmt.Errorf("продление сертификата: %w", err)
	}
	if err := saveCert(st.dir, resp.GetClientCertDer()); err != nil {
		return err
	}
	st.cert.Certificate = [][]byte{resp.GetClientCertDer()}
	return errRenewed
}

func saveCert(dir string, der []byte) error {
	tmp := filepath.Join(dir, certFile+".new")
	if err := os.WriteFile(tmp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, certFile))
}
