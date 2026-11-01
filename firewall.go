package main

import (
	"fmt"
	"net/netip"
	"strings"
	"sync/atomic"

	agentv1 "github.com/ZeniFlow/zeniflow-agent/proto/agent/v1"
)

// firewallState — состояние правил фаервола на ноде для heartbeat: ok, failed (не применились) или unsupported (не Linux).
var firewallState atomic.Value

func init() { firewallState.Store("unsupported") }

func currentFirewall() string { return firewallState.Load().(string) }

// smtpPorts — исходящая почта клиентов запрещена по умолчанию (защита от рассылки спама с наших адресов).
var smtpPorts = "25, 465, 587"

// overlayNet — сеть overlay: всё, что внутри неё, для VM закрыто, кроме явно разрешённого политикой.
const overlayNet = "10.99.0.0/16"

// currentPolicy — последняя сетевая политика от панели (nil, пока не пришла).
var currentPolicy atomic.Pointer[agentv1.NetworkPolicy]

func setElements(b *strings.Builder, name, typ string, elems []string) {
	fmt.Fprintf(b, "  set %s {\n    type %s\n", name, typ)
	if len(elems) > 0 {
		fmt.Fprintf(b, "    elements = { %s }\n", strings.Join(elems, ", "))
	}
	b.WriteString("  }\n")
}

// validIP отсекает всё, что не адрес IPv4: значения идут из панели, но в текст правил они попадают как есть.
func validIP(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && a.Is4()
}

// nftRules возвращает правила nftables. Таблицы создаются заново целиком одной транзакцией («объявить, удалить, объявить»):
// повторное применение безопасно и не оставляет старых правил. Трафик самой ноды почти не затрагивается: правила на пересылке,
// то есть на трафике VM и контейнеров, и одно правило на входе против обращений VM к самой ноде по адресу overlay.
// Политика (pol) задаёт, что VM можно в overlay: свои VM, свои базы и сайты; шлюзам можно обращаться к VM. Остальное закрыто.
// Без политики остаются только правила про почту.
func nftRules(pol *agentv1.NetworkPolicy) string {
	var b strings.Builder
	b.WriteString("table inet zeniflow\ndelete table inet zeniflow\ntable inet zeniflow {\n")
	if pol != nil {
		var vms, infra, pairs, ports []string
		for _, ip := range pol.GetLocalVmIps() {
			if validIP(ip) {
				vms = append(vms, ip)
			}
		}
		for _, ip := range pol.GetGatewayIps() {
			if validIP(ip) {
				infra = append(infra, ip)
			}
		}
		for _, p := range pol.GetVmPairs() {
			if validIP(p.GetSrc()) && validIP(p.GetDst()) {
				pairs = append(pairs, p.GetSrc()+" . "+p.GetDst())
			}
		}
		for _, p := range pol.GetPortPairs() {
			if validIP(p.GetSrc()) && validIP(p.GetDst()) && p.GetPort() > 0 && p.GetPort() < 65536 {
				ports = append(ports, fmt.Sprintf("%s . %s . %d", p.GetSrc(), p.GetDst(), p.GetPort()))
			}
		}
		setElements(&b, "vm_ips", "ipv4_addr", vms)
		setElements(&b, "infra_ips", "ipv4_addr", infra)
		setElements(&b, "vm_pairs", "ipv4_addr . ipv4_addr", pairs)
		setElements(&b, "vm_ports", "ipv4_addr . ipv4_addr . inet_service", ports)
	}
	b.WriteString("  chain forward {\n    type filter hook forward priority -10; policy accept;\n")
	b.WriteString("    tcp dport { " + smtpPorts + " } counter drop comment \"zeniflow: исходящая почта запрещена\"\n")
	if pol != nil {
		b.WriteString("    ct state established,related counter accept\n")
		b.WriteString("    ip saddr . ip daddr @vm_pairs counter accept\n")
		b.WriteString("    ip saddr . ip daddr . tcp dport @vm_ports counter accept\n")
		b.WriteString("    ip saddr @infra_ips ip daddr @vm_ips counter accept\n")
		b.WriteString("    ip saddr @vm_ips ip daddr " + overlayNet + " counter drop comment \"zeniflow: VM к чужому overlay\"\n")
		b.WriteString("    ip saddr " + overlayNet + " ip daddr @vm_ips counter drop comment \"zeniflow: чужой overlay к VM\"\n")
		// Контейнеры (сайты, базы) к VM не ходят: с адреса контейнера нельзя понять, чей он клиент.
		b.WriteString("    iifname \"docker0\" ip daddr @vm_ips counter drop\n")
		b.WriteString("    iifname \"br-*\" ip daddr @vm_ips counter drop\n")
	}
	b.WriteString("  }\n")
	if pol != nil && validIP(pol.GetNodeIp()) {
		b.WriteString("  chain input {\n    type filter hook input priority -10; policy accept;\n")
		b.WriteString("    ct state established,related accept\n")
		b.WriteString("    ip saddr . ip daddr . tcp dport @vm_ports accept\n")
		b.WriteString("    ip saddr @vm_ips ip daddr " + pol.GetNodeIp() + " counter drop comment \"zeniflow: VM к самой ноде\"\n  }\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// nftBridgeRules — то же для VM на мосту. Применяется отдельно: на ядре без модуля моста inet-часть должна работать всё равно.
func nftBridgeRules() string {
	var b strings.Builder
	b.WriteString("table bridge zeniflow\ndelete table bridge zeniflow\ntable bridge zeniflow {\n")
	b.WriteString("  chain forward {\n    type filter hook forward priority -10; policy accept;\n")
	// Порты VM друг с другом не общаются: между двумя tap-портами кадры бросаются. К ноде и к внешней сети (порт моста не tap) можно.
	// Свои VM клиента видят друг друга через маршрутизацию ноды (proxy ARP) и правила политики, а не напрямую по мосту.
	b.WriteString("    iifname \"tap*\" oifname \"tap*\" counter drop comment \"zeniflow: VM разных портов моста не общаются напрямую\"\n")
	b.WriteString("    ether type ip ip protocol tcp tcp dport { " + smtpPorts + " } counter drop\n")
	b.WriteString("    ether type ip6 ip6 nexthdr tcp tcp dport { " + smtpPorts + " } counter drop\n  }\n}\n")
	return b.String()
}
