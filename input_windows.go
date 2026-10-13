//go:build windows

package main

import (
	"image"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/kbinani/screenshot"
)

// ---- Monitor selecionado (streamMonitor vem de remote.go) ----------
func currentMonitor() int {
	n := int(atomic.LoadInt32(&streamMonitor))
	if n < 0 || n >= screenshot.NumActiveDisplays() {
		return 0
	}
	return n
}

// ---- Captura de tela -----------------------------------------------
func captureFrame(display int) image.Image {
	n := screenshot.NumActiveDisplays()
	if n == 0 {
		return nil
	}
	if display < 0 || display >= n {
		display = 0
	}
	img, err := screenshot.CaptureDisplay(display)
	if err != nil {
		return nil
	}
	return img
}

// ---- Injeção de mouse/teclado (SendInput) --------------------------

var (
	user32           = syscall.NewLazyDLL("user32.dll")
	procSendInput    = user32.NewProc("SendInput")
	procGetSysMetric = user32.NewProc("GetSystemMetrics")
)

const (
	inputMouse    = 0
	inputKeyboard = 1

	mouseeventfMove        = 0x0001
	mouseeventfAbsolute    = 0x8000
	mouseeventfVirtualDesk = 0x4000
	mouseeventfLeftDown    = 0x0002
	mouseeventfLeftUp      = 0x0004
	mouseeventfRightDown   = 0x0008
	mouseeventfRightUp     = 0x0010
	mouseeventfMiddleDown  = 0x0020
	mouseeventfMiddleUp    = 0x0040
	mouseeventfWheel       = 0x0800

	keyeventfKeyUp   = 0x0002
	keyeventfUnicode = 0x0004

	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79
)

// Estruturas INPUT com 40 bytes (amd64).
type mouseInput struct {
	typ         uint32
	_           [4]byte
	dx          int32
	dy          int32
	mouseData   uint32
	dwFlags     uint32
	time        uint32
	_           [4]byte
	dwExtraInfo uintptr
}

type kbInput struct {
	typ         uint32
	_           [4]byte
	wVk         uint16
	wScan       uint16
	dwFlags     uint32
	time        uint32
	_           [4]byte
	dwExtraInfo uintptr
	_           [8]byte
}

func sysMetric(i int) int {
	r, _, _ := procGetSysMetric.Call(uintptr(i))
	return int(int32(r))
}

func sendMouse(m mouseInput) {
	m.typ = inputMouse
	procSendInput.Call(1, uintptr(unsafe.Pointer(&m)), unsafe.Sizeof(m))
}

func sendKey(k kbInput) {
	k.typ = inputKeyboard
	procSendInput.Call(1, uintptr(unsafe.Pointer(&k)), unsafe.Sizeof(k))
}

// Converte (x,y) normalizado dentro do monitor para coordenada absoluta
// do desktop virtual (0..65535), para o SendInput.
func moveMouse(xn, yn float64) {
	disp := currentMonitor()
	b := screenshot.GetDisplayBounds(disp)
	screenX := float64(b.Min.X) + xn*float64(b.Dx())
	screenY := float64(b.Min.Y) + yn*float64(b.Dy())

	vx := sysMetric(smXVirtualScreen)
	vy := sysMetric(smYVirtualScreen)
	vw := sysMetric(smCXVirtualScreen)
	vh := sysMetric(smCYVirtualScreen)
	if vw <= 1 || vh <= 1 {
		return
	}
	absX := int32((screenX - float64(vx)) * 65535.0 / float64(vw-1))
	absY := int32((screenY - float64(vy)) * 65535.0 / float64(vh-1))

	sendMouse(mouseInput{
		dx: absX, dy: absY,
		dwFlags: mouseeventfMove | mouseeventfAbsolute | mouseeventfVirtualDesk,
	})
}

func injectInput(ev inputEvent) {
	switch ev.T {
	case "move":
		moveMouse(ev.X, ev.Y)
	case "down", "up":
		moveMouse(ev.X, ev.Y)
		var flag uint32
		switch ev.Button {
		case "right":
			flag = pick(ev.T, mouseeventfRightDown, mouseeventfRightUp)
		case "middle":
			flag = pick(ev.T, mouseeventfMiddleDown, mouseeventfMiddleUp)
		default:
			flag = pick(ev.T, mouseeventfLeftDown, mouseeventfLeftUp)
		}
		sendMouse(mouseInput{dwFlags: flag})
	case "scroll":
		sendMouse(mouseInput{mouseData: uint32(int32(-ev.Delta)), dwFlags: mouseeventfWheel})
	case "text":
		for _, r := range ev.Text {
			typeRune(r)
		}
	case "key":
		typeKey(ev)
	}
}

func pick(t string, down, up uint32) uint32 {
	if t == "down" {
		return down
	}
	return up
}

func typeRune(r rune) {
	sendKey(kbInput{wScan: uint16(r), dwFlags: keyeventfUnicode})
	sendKey(kbInput{wScan: uint16(r), dwFlags: keyeventfUnicode | keyeventfKeyUp})
}

// Teclas especiais e atalhos.
func typeKey(ev inputEvent) {
	vk, ok := vkMap[ev.Key]
	if !ok {
		// Tecla imprimível sem mapeamento especial: trata como texto no keydown
		if ev.Down && len(ev.Key) == 1 && !ev.Ctrl && !ev.Alt {
			typeRune(rune(ev.Key[0]))
		}
		return
	}
	if ev.Down {
		if ev.Ctrl {
			sendKey(kbInput{wVk: 0x11})
		}
		if ev.Shift {
			sendKey(kbInput{wVk: 0x10})
		}
		if ev.Alt {
			sendKey(kbInput{wVk: 0x12})
		}
		sendKey(kbInput{wVk: vk})
		sendKey(kbInput{wVk: vk, dwFlags: keyeventfKeyUp})
		if ev.Alt {
			sendKey(kbInput{wVk: 0x12, dwFlags: keyeventfKeyUp})
		}
		if ev.Shift {
			sendKey(kbInput{wVk: 0x10, dwFlags: keyeventfKeyUp})
		}
		if ev.Ctrl {
			sendKey(kbInput{wVk: 0x11, dwFlags: keyeventfKeyUp})
		}
	}
}

var vkMap = map[string]uint16{
	"Enter": 0x0D, "Backspace": 0x08, "Tab": 0x09, "Escape": 0x1B,
	"Delete": 0x2E, "Insert": 0x2D, "Home": 0x24, "End": 0x23,
	"PageUp": 0x21, "PageDown": 0x22, "ArrowLeft": 0x25, "ArrowUp": 0x26,
	"ArrowRight": 0x27, "ArrowDown": 0x28, " ": 0x20,
	"F1": 0x70, "F2": 0x71, "F3": 0x72, "F4": 0x73, "F5": 0x74, "F6": 0x75,
	"F7": 0x76, "F8": 0x77, "F9": 0x78, "F10": 0x79, "F11": 0x7A, "F12": 0x7B,
	// Letras e dígitos para atalhos (Ctrl+C etc.)
	"a": 0x41, "b": 0x42, "c": 0x43, "d": 0x44, "e": 0x45, "f": 0x46, "g": 0x47,
	"h": 0x48, "i": 0x49, "j": 0x4A, "k": 0x4B, "l": 0x4C, "m": 0x4D, "n": 0x4E,
	"o": 0x4F, "p": 0x50, "q": 0x51, "r": 0x52, "s": 0x53, "t": 0x54, "u": 0x55,
	"v": 0x56, "w": 0x57, "x": 0x58, "y": 0x59, "z": 0x5A,
	"0": 0x30, "1": 0x31, "2": 0x32, "3": 0x33, "4": 0x34,
	"5": 0x35, "6": 0x36, "7": 0x37, "8": 0x38, "9": 0x39,
}
