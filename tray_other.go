//go:build !windows

package main

// Fora do Windows não há bandeja; apenas mantém o processo vivo.
func runTray(c *Config) { select {} }
