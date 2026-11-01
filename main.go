// zeniflow-agent: агент на ноде. Регистрируется в панели по одноразовому токену и держит с ней защищённый поток.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// version подставляется при сборке: go build -ldflags "-X main.version=1.2.3".
var version = "0.1.0"

const usage = `zeniflow-agent — агент ноды ZeniFlow Cloud

Команды:
  enroll   зарегистрировать ноду в панели (--server, --token, --ca-sha256)
  proxmox  сохранить доступ к API Proxmox на этой ноде (--node, --token, --url)
  firewall показать правила фаервола nftables; «firewall apply» — применить их (нужен root, Linux)
  run      подключиться к панели и работать (по умолчанию)
  metrics  показать данные о сервере, которые агент отправляет панели
  version  показать версию

Общий флаг: --state-dir каталог с ключами и настройками (по умолчанию /etc/zeniflow-agent для root, иначе ~/.zeniflow-agent)
`

func main() {
	cmd := "run"
	args := os.Args[1:]
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}

	var err error
	switch cmd {
	case "enroll":
		err = cmdEnroll(args)
	case "proxmox":
		err = cmdProxmox(args)
	case "firewall":
		err = cmdFirewall(args)
	case "run":
		err = cmdRun(args)
	case "metrics":
		err = cmdMetrics()
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "неизвестная команда %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		slog.Error("ошибка", "err", err)
		os.Exit(1)
	}
}

func cmdMetrics() error {
	snap := collect()
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(snap)
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	dir := fs.String("state-dir", defaultStateDir(), "каталог состояния")
	_ = fs.Parse(args)

	st, err := loadState(*dir)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runLoop(ctx, st)
}

func cmdFirewall(args []string) error {
	fs := flag.NewFlagSet("firewall", flag.ExitOnError)
	dir := fs.String("state-dir", defaultStateDir(), "каталог состояния: оттуда берётся сохранённая сетевая политика")
	apply := len(args) > 0 && args[0] == "apply"
	if apply {
		args = args[1:]
	}
	_ = fs.Parse(args)
	if p := loadPolicy(*dir); p != nil {
		currentPolicy.Store(p)
	}
	if apply {
		applyFirewall(context.Background())
		fmt.Println("состояние:", currentFirewall())
		if currentFirewall() != "ok" {
			return errors.New("правила не применены, подробности в журнале выше")
		}
		return nil
	}
	fmt.Print(nftRules(currentPolicy.Load()), "\n", nftBridgeRules())
	return nil
}
