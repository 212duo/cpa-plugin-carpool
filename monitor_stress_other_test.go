//go:build !linux && !windows

package main

// Detailed CPU/RSS measurements are collected by the Linux load-test run.
func monitorStressUsage() (float64, int64) { return 0, 0 }
