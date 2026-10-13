//go:build windows

package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// installSelf copia o agente para uma pasta fixa e registra o autoinício
// no login do usuário (HKCU...\Run). Se estiver rodando de outro lugar
// (ex.: Downloads), copia-se e reinicia a partir do local fixo.
func installSelf() {
	target := installTarget()
	cur, err := os.Executable()
	if err != nil {
		return
	}
	cur, _ = filepath.Abs(cur)

	// Garante o autoinício apontando para o local fixo.
	setRunKey(target)

	if samePath(cur, target) {
		return // já estamos no local certo
	}

	// Copia o executável atual para o local fixo.
	if err := copyFile(cur, target); err != nil {
		logf("[install] falha ao copiar para %s: %v", target, err)
		return
	}
	logf("[install] instalado em %s; reiniciando a partir de lá", target)

	// Inicia a cópia e encerra este processo.
	c := exec.Command(target)
	c.SysProcAttr = hideWindow()
	_ = c.Start()
	os.Exit(0)
}

func installTarget() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.Getenv("APPDATA")
	}
	dir := filepath.Join(base, "Hub360Agente")
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "AgenteSuporte.exe")
}

func samePath(a, b string) bool {
	ab, _ := filepath.Abs(a)
	bb, _ := filepath.Abs(b)
	return equalFold(ab, bb)
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 32
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 32
		}
		if ca == '/' {
			ca = '\\'
		}
		if cb == '/' {
			cb = '\\'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// setRunKey grava a chave de autoinício do usuário via reg.exe.
func setRunKey(target string) {
	c := exec.Command("reg", "add",
		`HKCU\Software\Microsoft\Windows\CurrentVersion\Run`,
		"/v", "Hub360Agente", "/t", "REG_SZ",
		"/d", `"`+target+`"`, "/f")
	c.SysProcAttr = hideWindow()
	_ = c.Run()
}
