package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"flag"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	agentv1 "github.com/ZeniFlow/zeniflow-agent/proto/agent/v1"
)

func cmdEnroll(args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	server := fs.String("server", "", "адрес панели для агентов, host:port")
	token := fs.String("token", "", "одноразовый токен из панели")
	caSHA := fs.String("ca-sha256", "", "отпечаток CA панели из команды подключения")
	dir := fs.String("state-dir", defaultStateDir(), "каталог состояния")
	_ = fs.Parse(args)

	if *server == "" || *token == "" || *caSHA == "" {
		return errors.New("нужны --server, --token и --ca-sha256: скопируйте готовую команду из панели")
	}
	host, _, err := net.SplitHostPort(*server)
	if err != nil {
		return fmt.Errorf("--server должен быть в виде host:port: %w", err)
	}
	if isEnrolled(*dir) {
		return fmt.Errorf("нода уже зарегистрирована (каталог %s). Чтобы зарегистрировать заново, удалите каталог", *dir)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "zeniflow-agent"},
	}, key)
	if err != nil {
		return err
	}

	conn, err := grpc.NewClient(*server, grpc.WithTransportCredentials(credentials.NewTLS(pinnedTLS(host, *caSHA))))
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := agentv1.NewAgentServiceClient(conn).Enroll(ctx, &agentv1.EnrollRequest{
		Token:  *token,
		CsrDer: csr,
		Info:   collect().info(),
	})
	if err != nil {
		return fmt.Errorf("регистрация не удалась: %w", err)
	}

	cfg := nodeConfig{Server: *server, NodeID: resp.GetNodeId()}
	if err := saveState(*dir, cfg, key, resp.GetClientCertDer(), resp.GetCaCertDer()); err != nil {
		return err
	}
	fmt.Printf("Нода зарегистрирована: %s\nДанные сохранены в %s\nЗапустите агента: zeniflow-agent run\n", resp.GetNodeId(), *dir)
	return nil
}
