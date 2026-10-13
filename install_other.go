//go:build !windows

package main

// Fora do Windows não há autoinício nem bandeja.
func installSelf() {}
