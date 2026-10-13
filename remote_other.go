//go:build !windows

package main

import "image"

// Versões vazias para compilar fora do Windows (o build real é Windows).
func currentMonitor() int                  { return 0 }
func captureFrame(display int) image.Image { return nil }
func injectInput(ev inputEvent)            {}
