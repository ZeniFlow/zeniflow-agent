package main

import (
	"os"
	"syscall"
)

func readMemory() (totalMB, usedMB int64) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	return parseMeminfo(string(raw))
}

func readLoad1() float64 {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	return parseLoad1(string(raw))
}

// readDisk возвращает размер и занятое место файловой системы по пути path, в гигабайтах.
func readDisk(path string) (totalGB, usedGB int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	const gb = 1 << 30
	bsize := uint64(st.Bsize)
	total := st.Blocks * bsize
	used := (st.Blocks - st.Bfree) * bsize
	return int64(total / gb), int64(used / gb)
}
