package main

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Файлы состояния лежат в одном каталоге. Закрытый ключ не покидает ноду и доступен только владельцу.
const (
	nodeFile = "node.json"
	keyFile  = "client.key"
	certFile = "client.crt"
	caFile   = "ca.crt"
)

type nodeConfig struct {
	Server string `json:"server"`
	NodeID string `json:"node_id"`
}

type state struct {
	dir    string
	cfg    nodeConfig
	cert   tls.Certificate
	caPool *x509.CertPool
	wgPriv string // закрытый ключ WireGuard (base64)
	wgPub  string
}

func defaultStateDir() string {
	if os.Geteuid() == 0 {
		return "/etc/zeniflow-agent"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".zeniflow-agent"
	}
	return filepath.Join(home, ".zeniflow-agent")
}

func isEnrolled(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, nodeFile))
	return err == nil
}

func saveState(dir string, cfg nodeConfig, key *ecdsa.PrivateKey, certDER, caDER []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	cfgJSON, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	files := []struct {
		name string
		data []byte
	}{
		{keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})},
		{certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})},
		{caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})},
		{nodeFile, cfgJSON}, // последним: по его наличию агент считает себя зарегистрированным
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.name), f.data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func loadState(dir string) (*state, error) {
	raw, err := os.ReadFile(filepath.Join(dir, nodeFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("нода не зарегистрирована (нет %s): сначала выполните zeniflow-agent enroll", filepath.Join(dir, nodeFile))
	}
	if err != nil {
		return nil, err
	}
	st := &state{dir: dir}
	if err := json.Unmarshal(raw, &st.cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", nodeFile, err)
	}
	st.cert, err = tls.LoadX509KeyPair(filepath.Join(dir, certFile), filepath.Join(dir, keyFile))
	if err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, caFile))
	if err != nil {
		return nil, err
	}
	st.caPool = x509.NewCertPool()
	if !st.caPool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("ca.crt не содержит сертификата")
	}
	st.wgPriv, st.wgPub, err = loadOrCreateWG(dir)
	if err != nil {
		return nil, err
	}
	return st, nil
}
