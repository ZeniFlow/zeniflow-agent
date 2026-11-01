package main

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	agentv1 "github.com/ZeniFlow/zeniflow-agent/proto/agent/v1"
)

const (
	heartbeatEvery = 15 * time.Second
	maxBackoff     = time.Minute
)

// runLoop держит соединение с панелью. При обрыве переподключается с нарастающей паузой: ноде не нужно ничего делать вручную.
func runLoop(ctx context.Context, st *state) error {
	host, _, err := net.SplitHostPort(st.cfg.Server)
	if err != nil {
		return err
	}
	slog.Info("агент запущен", "version", version, "node", st.cfg.NodeID, "server", st.cfg.Server)

	if p := loadPolicy(st.dir); p != nil {
		currentPolicy.Store(p) // правила изоляции действуют сразу после перезапуска, до связи с панелью
	}
	go watchFirewall(ctx)
	tm := newTunnelManager(ctx, st) // туннели живут дольше одного подключения к панели
	backoff := time.Second
	for {
		started := time.Now()
		err := session(ctx, st, host, tm)
		if ctx.Err() != nil {
			slog.Info("агент остановлен")
			return nil
		}
		if time.Since(started) > 30*time.Second {
			backoff = time.Second // соединение было стабильным, начинаем с короткой паузы
		}
		slog.Warn("связь с панелью потеряна", "err", err, "повтор_через", backoff.String())
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// session — одно подключение: открывает поток и шлёт heartbeat, пока поток жив.
func session(ctx context.Context, st *state, host string, tm *tunnelManager) error {
	conn, err := grpc.NewClient(st.cfg.Server,
		grpc.WithTransportCredentials(credentials.NewTLS(mutualTLS(st, host))),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true}),
	)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	client := agentv1.NewAgentServiceClient(conn)
	if err := renewIfDue(ctx, st, client); err != nil {
		return err
	}
	stream, err := client.Connect(ctx)
	if err != nil {
		return err
	}

	// gRPC не допускает параллельных Send: heartbeat и результаты команд идут через один замок.
	var sendMu sync.Mutex
	sendMsg := func(m *agentv1.AgentMessage) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(m)
	}
	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			if cmd := msg.GetCommand(); cmd != nil {
				go func() { // создание VM занимает минуты, поток при этом должен продолжать работать
					res := execute(ctx, st.dir, cmd)
					if err := sendMsg(&agentv1.AgentMessage{Payload: &agentv1.AgentMessage_CommandResult{CommandResult: res}}); err != nil {
						slog.Warn("результат команды не отправлен", "id", cmd.GetId(), "err", err)
					}
				}()
			}
			if p := msg.GetPolicy(); p != nil {
				onPolicy(ctx, st.dir, p)
			}
			if t := msg.GetTunnel(); t != nil {
				tm.set(t.GetServers())
			}
			if cfg := msg.GetOverlay(); cfg != nil {
				if err := applyOverlay(st, cfg); err != nil {
					slog.Error("overlay не применён", "err", err) // не обрываем связь: панель пришлёт конфигурацию при следующем изменении
				} else {
					slog.Info("overlay применён", "address", cfg.GetAddress(), "peers", len(cfg.GetPeers()))
				}
			}
		}
	}()

	send := func() error {
		hb := collect().heartbeat()
		hb.Info.WgPublicKey = st.wgPub
		return sendMsg(&agentv1.AgentMessage{Payload: &agentv1.AgentMessage_Heartbeat{Heartbeat: hb}})
	}
	if err := send(); err != nil {
		return err
	}
	slog.Info("подключено к панели")

	ticker := time.NewTicker(heartbeatEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = stream.CloseSend()
			return ctx.Err()
		case err := <-recvErr:
			return err
		case <-ticker.C:
			if err := send(); err != nil {
				return err
			}
			if err := renewIfDue(ctx, st, client); err != nil {
				return err
			}
		}
	}
}
