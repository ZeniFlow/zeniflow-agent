//go:build !linux

package main

// Ноды работают на Linux. На других системах (например, при разработке на Mac) память, диск и нагрузка не собираются.

func readMemory() (totalMB, usedMB int64)     { return 0, 0 }
func readLoad1() float64                      { return 0 }
func readDisk(string) (totalGB, usedGB int64) { return 0, 0 }
