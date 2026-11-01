# ZeniFlow Agent

The agent runs on a node (a VPS, a bare-metal server or a Proxmox host) and connects it to a [ZeniFlow Cloud](https://cloud.zeniflow.ru) panel. It is a single static Go binary with no runtime dependencies.

**Status: early.** Docker workloads (sites, databases), metrics, the nftables firewall, the WireGuard overlay and the tunnel work in tests. Proxmox virtual machines are implemented but have not yet been verified against a real Proxmox host. Expect changes.

## Install (one command)

In the panel open **Admin → Nodes → Add node** and copy the command. It looks like this:

```bash
curl -fsSL https://raw.githubusercontent.com/ZeniFlow/zeniflow-agent/main/install.sh | sudo bash -s -- \
  --server panel.example.com:8443 --token TOKEN --ca-sha256 HEX
```

The script downloads the release binary for your architecture (linux amd64 or arm64), **verifies its SHA-256 against `checksums.txt`**, registers the node with the one-time token, and installs a systemd service. Remove the agent with `sudo bash install.sh --uninstall`.

Prefer to read before you run? The script is [install.sh](install.sh), about 90 lines. You can also build from source (below) and pass `--binary /path/to/zeniflow-agent`.

## How it works

- **Outbound only.** The agent dials the panel (default port 8443) and keeps one bidirectional gRPC stream. The node needs no open inbound port, so it works behind NAT.
- **Enrollment.** `enroll` takes a one-time token and the SHA-256 fingerprint of the panel's CA. The agent pins that CA, generates a P-256 key locally, sends a CSR and receives a client certificate. The private key never leaves the node. The certificate renews automatically.
- **Work.** Over the stream the panel sends commands (deploy a site or a database in Docker, manage a Proxmox VM, back up, apply firewall policy); the agent reports metrics and results.
- **Security model.** The agent runs as root and obeys the panel it is enrolled to, authenticated with mutual TLS. A compromised panel is a compromised node: enroll nodes only into a panel you trust. The protocol is in [proto/agent/v1/agent.proto](proto/agent/v1/agent.proto).

## Commands

```
zeniflow-agent enroll   --server HOST:PORT --token TOKEN --ca-sha256 HEX
zeniflow-agent run      connect to the panel and work (default)
zeniflow-agent proxmox  --node NAME --token TOKEN --url URL   save Proxmox API access
zeniflow-agent firewall [apply]   show or apply the nftables rules
zeniflow-agent metrics  print what the agent reports
zeniflow-agent version
```

State (key, certificate, config) lives in `/etc/zeniflow-agent` (`~/.zeniflow-agent` when not root).

## Build from source

Go 1.26 or newer:

```bash
go build -trimpath -ldflags="-s -w" -o zeniflow-agent .
go test ./...
```

## Releases

Pushing a tag `vX.Y.Z` builds linux/amd64 and linux/arm64 binaries and `checksums.txt` and publishes them as a GitHub release.

## License

Apache-2.0, see [LICENSE](LICENSE).

---

## По-русски

Агент ZeniFlow подключает ноду (VPS, сервер или хост Proxmox) к панели ZeniFlow Cloud. Это один статический бинарник на Go.

**Установка одной командой:** скопируйте готовую команду в панели («Админка → Ноды → Добавить ноду») и выполните на ноде от root. Скрипт скачает бинарник, **сверит его контрольную сумму**, зарегистрирует ноду по одноразовому токену и поставит службу systemd. Агент сам подключается к панели (исходящее соединение), входящие порты не нужны.

**Безопасность:** агент работает от root и выполняет команды панели, к которой подключён (взаимная проверка TLS). Подключайте ноды только к панели, которой доверяете. Сообщить об уязвимости: [SECURITY.md](SECURITY.md).

**Статус:** ранний. Виртуальные машины Proxmox реализованы, но ещё не проверены на настоящем Proxmox.
