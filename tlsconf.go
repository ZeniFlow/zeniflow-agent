package main

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// pinnedTLS — TLS для первого подключения, пока у агента ещё нет сертификата CA. Панель присылает цепочку вместе
// с CA, агент находит в ней сертификат с отпечатком из команды подключения и проверяет по нему сервер.
// Так подмена панели по пути невозможна: отпечаток есть только в команде, которую выдала настоящая панель.
func pinnedTLS(host, caSHA256 string) *tls.Config {
	want := strings.ToLower(strings.TrimSpace(caSHA256))
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// Стандартная проверка отключена намеренно: вместо неё VerifyPeerCertificate делает проверку по закреплённому CA.
		InsecureSkipVerify: true, //nolint:gosec
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("сервер не прислал сертификат")
			}
			certs := make([]*x509.Certificate, len(raw))
			var ca *x509.Certificate
			for i, der := range raw {
				c, err := x509.ParseCertificate(der)
				if err != nil {
					return err
				}
				certs[i] = c
				sum := sha256.Sum256(der)
				if hex.EncodeToString(sum[:]) == want {
					ca = c
				}
			}
			if ca == nil {
				return errors.New("отпечаток CA не совпал с указанным в команде: возможно, это не ваша панель")
			}
			roots := x509.NewCertPool()
			roots.AddCert(ca)
			_, err := certs[0].Verify(x509.VerifyOptions{
				Roots:     roots,
				DNSName:   host,
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			})
			if err != nil {
				return fmt.Errorf("сертификат сервера не прошёл проверку: %w", err)
			}
			return nil
		},
	}
}

// mutualTLS — обычное подключение после регистрации: сервер проверяется по сохранённому CA, агент предъявляет свой сертификат.
func mutualTLS(st *state, host string) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		RootCAs:      st.caPool,
		ServerName:   host,
		Certificates: []tls.Certificate{st.cert},
	}
}
