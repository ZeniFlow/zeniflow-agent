package main

import "testing"

func TestParseMeminfo(t *testing.T) {
	const sample = `MemTotal:       16384000 kB
MemFree:         1000000 kB
MemAvailable:    8192000 kB
Buffers:          100000 kB
`
	total, used := parseMeminfo(sample)
	if total != 16000 || used != 8000 {
		t.Errorf("получили total=%d used=%d, ждали 16000 и 8000", total, used)
	}
	if total, used := parseMeminfo("мусор\nMemTotal: abc kB\n"); total != 0 || used != 0 {
		t.Errorf("мусор должен давать нули, получили %d %d", total, used)
	}
}

func TestParseLoad1(t *testing.T) {
	if got := parseLoad1("0.52 0.40 0.31 1/234 5678\n"); got != 0.52 {
		t.Errorf("получили %v", got)
	}
	if got := parseLoad1(""); got != 0 {
		t.Errorf("пустая строка должна давать 0, получили %v", got)
	}
}
