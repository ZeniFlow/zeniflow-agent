package main

import (
	"reflect"
	"testing"
)

func TestStaleRoutes(t *testing.T) {
	out := `192.168.1.0/24 proto kernel scope link src 192.168.1.10
10.99.0.5 scope link
10.99.0.6/32 scope link
10.99.0.0/24 scope link
10.99.0.7 scope link
`
	got := staleRoutes(out, map[string]bool{"10.99.0.5": true})
	if want := []string{"10.99.0.6", "10.99.0.7"}; !reflect.DeepEqual(got, want) {
		t.Errorf("устаревшие маршруты: %v, ждали %v", got, want)
	}
	if !bridgeRe.MatchString("vmbr0") || bridgeRe.MatchString("../etc") || bridgeRe.MatchString("a b") || bridgeRe.MatchString("") {
		t.Error("проверка имени моста")
	}
}
