package main

import (
	"regexp"
	"strings"
)

// bridgeRe — допустимое имя сетевого интерфейса Linux (до 15 символов), чтобы оно безопасно попадало в пути /proc и команды.
var bridgeRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,15}$`)

// staleRoutes разбирает вывод `ip -o -4 route show dev <мост>` и возвращает адреса overlay, для которых на мосту остался маршрут,
// а VM на ноде больше нет. Чужие маршруты (не из пула 10.99.) не трогаем.
func staleRoutes(output string, want map[string]bool) []string {
	var stale []string
	for _, line := range strings.Split(output, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		ip := strings.TrimSuffix(f[0], "/32")
		if strings.HasPrefix(ip, "10.99.") && !strings.Contains(ip, "/") && !want[ip] {
			stale = append(stale, ip)
		}
	}
	return stale
}
