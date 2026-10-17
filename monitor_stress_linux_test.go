//go:build linux

package main

import "syscall"

func monitorStressUsage() (cpuSeconds float64, peakRSSBytes int64) {
	var r syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &r) == nil {
		cpuSeconds = float64(r.Utime.Sec+r.Stime.Sec) + float64(r.Utime.Usec+r.Stime.Usec)/1e6
		peakRSSBytes = r.Maxrss * 1024
	}
	return
}
