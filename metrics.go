package main

import (
	"os"
	"runtime"
	"strconv"
	"strings"

	agentv1 "github.com/ZeniFlow/zeniflow-agent/proto/agent/v1"
)

// snapshot — то, что агент сообщает панели о сервере.
type snapshot struct {
	Hostname     string  `json:"hostname"`
	OS           string  `json:"os"`
	Arch         string  `json:"arch"`
	AgentVersion string  `json:"agent_version"`
	CPUCores     int     `json:"cpu_cores"`
	RAMTotalMB   int64   `json:"ram_total_mb"`
	RAMUsedMB    int64   `json:"ram_used_mb"`
	DiskTotalGB  int64   `json:"disk_total_gb"`
	DiskUsedGB   int64   `json:"disk_used_gb"`
	Load1        float64 `json:"load1"`
}

func collect() snapshot {
	host, _ := os.Hostname()
	s := snapshot{
		Hostname:     host,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		AgentVersion: version,
		CPUCores:     runtime.NumCPU(),
	}
	s.RAMTotalMB, s.RAMUsedMB = readMemory()
	s.DiskTotalGB, s.DiskUsedGB = readDisk("/")
	s.Load1 = readLoad1()
	return s
}

func (s snapshot) info() *agentv1.NodeInfo {
	return &agentv1.NodeInfo{
		Hostname:     s.Hostname,
		Os:           s.OS,
		Arch:         s.Arch,
		AgentVersion: s.AgentVersion,
		CpuCores:     int32(s.CPUCores),
		RamTotalMb:   s.RAMTotalMB,
		DiskTotalGb:  s.DiskTotalGB,
		Firewall:     currentFirewall(),
	}
}

func (s snapshot) heartbeat() *agentv1.Heartbeat {
	return &agentv1.Heartbeat{Info: s.info(), RamUsedMb: s.RAMUsedMB, DiskUsedGb: s.DiskUsedGB, Load1: s.Load1}
}

// parseMeminfo разбирает /proc/meminfo. Занято = всего минус доступно (MemAvailable учитывает кеш, которое ядро освободит).
func parseMeminfo(text string) (totalMB, usedMB int64) {
	var totalKB, availKB int64
	for _, line := range strings.Split(text, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch name {
		case "MemTotal":
			totalKB = kb
		case "MemAvailable":
			availKB = kb
		}
	}
	if totalKB == 0 {
		return 0, 0
	}
	return totalKB / 1024, (totalKB - availKB) / 1024
}

// parseLoad1 берёт среднюю нагрузку за минуту из /proc/loadavg.
func parseLoad1(text string) float64 {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return v
}
