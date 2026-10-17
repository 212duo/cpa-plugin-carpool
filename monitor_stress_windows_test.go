//go:build windows

package main

import "golang.org/x/sys/windows"

func monitorStressUsage() (cpuSeconds float64, peakRSSBytes int64) {
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(windows.CurrentProcess(), &created, &exited, &kernel, &user) == nil {
		cpuSeconds = (float64(uint64(kernel.HighDateTime)<<32|uint64(kernel.LowDateTime)) + float64(uint64(user.HighDateTime)<<32|uint64(user.LowDateTime))) / 1e7
	}
	// Peak RSS is recorded by the Linux run; Go heap is measured on both systems.
	return
}
