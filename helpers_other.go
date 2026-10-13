//go:build !windows

package main

import (
	"os"
	"syscall"
)

// Versões para compilar/testar fora do Windows (Linux, Mac).
func hideWindow() *syscall.SysProcAttr { return nil }

func machineUID() string {
	host, _ := os.Hostname()
	return "dev-" + host
}

func monitorCount() int { return 1 }
