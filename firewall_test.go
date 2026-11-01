package main

import (
	"strings"
	"testing"

	agentv1 "github.com/ZeniFlow/zeniflow-agent/proto/agent/v1"
)

func TestNftRulesWithoutPolicy(t *testing.T) {
	r := nftRules(nil)
	for _, want := range []string{"table inet zeniflow", "delete table inet zeniflow", "hook forward", "policy accept", "tcp dport { 25, 465, 587 }", "drop"} {
		if !strings.Contains(r, want) {
			t.Errorf("в правилах нет %q:\n%s", want, r)
		}
	}
	// Без политики правила только про почту: трафик ноды и VM друг с другом не трогается.
	for _, bad := range []string{"hook input", "hook output", "policy drop", "vm_ips", "10.99.0.0/16"} {
		if strings.Contains(r, bad) {
			t.Errorf("без политики не должно быть %q:\n%s", bad, r)
		}
	}
	b := nftBridgeRules()
	for _, want := range []string{"table bridge zeniflow", "ether type ip ip protocol tcp tcp dport { 25, 465, 587 }", "ip6 nexthdr tcp", "iifname \"tap*\" oifname \"tap*\""} {
		if !strings.Contains(b, want) {
			t.Errorf("в мостовых правилах нет %q:\n%s", want, b)
		}
	}
	if currentFirewall() == "" {
		t.Error("состояние фаервола должно быть задано")
	}
}

func TestNftRulesWithPolicy(t *testing.T) {
	pol := &agentv1.NetworkPolicy{
		NodeIp:     "10.99.0.1",
		LocalVmIps: []string{"10.99.0.10", "10.99.0.20"},
		GatewayIps: []string{"10.99.0.200"},
		VmPairs:    []*agentv1.IPPair{{Src: "10.99.0.10", Dst: "10.99.0.11"}, {Src: "10.99.0.11", Dst: "10.99.0.10"}},
		PortPairs:  []*agentv1.PortPair{{Src: "10.99.0.10", Dst: "10.99.0.1", Port: 30000}},
	}
	r := nftRules(pol)
	for _, want := range []string{
		"set vm_ips", "elements = { 10.99.0.10, 10.99.0.20 }",
		"elements = { 10.99.0.10 . 10.99.0.11, 10.99.0.11 . 10.99.0.10 }",
		"elements = { 10.99.0.10 . 10.99.0.1 . 30000 }",
		"elements = { 10.99.0.200 }",
		"ip saddr . ip daddr @vm_pairs counter accept",
		"ip saddr @vm_ips ip daddr 10.99.0.0/16 counter drop",
		"ip saddr 10.99.0.0/16 ip daddr @vm_ips counter drop",
		"chain input", "ip saddr @vm_ips ip daddr 10.99.0.1 counter drop",
	} {
		if !strings.Contains(r, want) {
			t.Errorf("в правилах нет %q:\n%s", want, r)
		}
	}
	// Разрешающие правила стоят раньше запрещающих, ответы на разрешённое тоже пропускаются.
	if strings.Index(r, "@vm_pairs counter accept") > strings.Index(r, "VM к чужому overlay") || !strings.Contains(r, "ct state established,related accept") {
		t.Errorf("порядок правил:\n%s", r)
	}

	// Пустая политика (VM нет): наборы объявлены без элементов, иначе nft откажет в синтаксисе.
	empty := nftRules(&agentv1.NetworkPolicy{})
	if strings.Contains(empty, "elements") || !strings.Contains(empty, "set vm_ips") || strings.Contains(empty, "chain input") {
		t.Errorf("пустая политика:\n%s", empty)
	}

	// Значения из панели не должны превращаться в чужие команды nft.
	evil := nftRules(&agentv1.NetworkPolicy{NodeIp: "10.99.0.1; flush ruleset", LocalVmIps: []string{"1.1.1.1 }; flush ruleset; {", "10.99.0.5"},
		VmPairs: []*agentv1.IPPair{{Src: "x", Dst: "10.99.0.5"}}, PortPairs: []*agentv1.PortPair{{Src: "10.99.0.5", Dst: "10.99.0.1", Port: 70000}}})
	if strings.Contains(evil, "flush") || strings.Contains(evil, "70000") || strings.Contains(evil, "x .") || !strings.Contains(evil, "10.99.0.5") {
		t.Errorf("подставленные значения прошли в правила:\n%s", evil)
	}
}

func TestPolicyPersists(t *testing.T) {
	dir := t.TempDir()
	if loadPolicy(dir) != nil {
		t.Fatal("политики ещё нет")
	}
	want := &agentv1.NetworkPolicy{NodeIp: "10.99.0.1", LocalVmIps: []string{"10.99.0.10"}, PortPairs: []*agentv1.PortPair{{Src: "10.99.0.10", Dst: "10.99.0.1", Port: 30000}}}
	if err := savePolicy(dir, want); err != nil {
		t.Fatal(err)
	}
	got := loadPolicy(dir)
	if got == nil || got.String() != want.String() {
		t.Errorf("после перезапуска агента политика должна вернуться: %v", got)
	}
}
