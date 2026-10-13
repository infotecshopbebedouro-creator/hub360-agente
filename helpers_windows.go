//go:build windows

package main

import (
	"strconv"
	"strings"
	"syscall"
)

// Esconde a janela preta do PowerShell/cmd em cada comando.
func hideWindow() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true}
}

// Identificador único da máquina: MachineGuid do registro do Windows.
func machineUID() string {
	uid := ps(`(Get-ItemProperty "HKLM:\SOFTWARE\Microsoft\Cryptography").MachineGuid`)
	if uid == "" {
		uid = ps(`(Get-CimInstance Win32_ComputerSystemProduct).UUID`)
	}
	return strings.TrimSpace(uid)
}

// Quantidade de monitores conectados.
func monitorCount() int {
	out := ps(`(Get-CimInstance -Namespace root\wmi -Class WmiMonitorBasicDisplayParams -ErrorAction SilentlyContinue | Measure-Object).Count`)
	if n, err := strconv.Atoi(strings.TrimSpace(out)); err == nil && n > 0 {
		return n
	}
	// Alternativa
	out = ps(`[System.Windows.Forms.Screen]::AllScreens.Count`)
	if n, err := strconv.Atoi(strings.TrimSpace(out)); err == nil && n > 0 {
		return n
	}
	return 1
}
