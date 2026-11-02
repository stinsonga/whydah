package clipboard

import (
	"runtime"
	"syscall"
	"unsafe"
)

var (
	user32                     = syscall.NewLazyDLL("user32.dll")
	kernel32                   = syscall.NewLazyDLL("kernel32.dll")
	isClipboardFormatAvailable = user32.NewProc("IsClipboardFormatAvailable")
	openClipboard              = user32.NewProc("OpenClipboard")
	closeClipboard             = user32.NewProc("CloseClipboard")
	getClipboardData           = user32.NewProc("GetClipboardData")
	globalLock                 = kernel32.NewProc("GlobalLock")
	globalUnlock               = kernel32.NewProc("GlobalUnlock")
)

const (
	cfUnicodeText = 13
	maxChars      = 1 << 16 // URLs are short; don't copy huge clipboard contents
)

// read uses the Win32 clipboard API directly (no console window, no cgo).
func read() (string, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if r, _, _ := isClipboardFormatAvailable.Call(cfUnicodeText); r == 0 {
		return "", nil
	}
	if r, _, _ := openClipboard.Call(0); r == 0 {
		return "", nil // another app holds the clipboard; try again next tick
	}
	defer closeClipboard.Call()

	h, _, _ := getClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return "", nil
	}
	p, _, _ := globalLock.Call(h)
	if p == 0 {
		return "", nil
	}
	defer globalUnlock.Call(h)

	// p is memory owned by Windows, not the Go heap, so the garbage collector
	// can't move it. Reinterpreting the variable, rather than converting
	// the uintptr, says so to vet.
	text := *(*unsafe.Pointer)(unsafe.Pointer(&p))
	var buf []uint16
	for ptr := text; len(buf) < maxChars; ptr = unsafe.Add(ptr, 2) {
		c := *(*uint16)(ptr)
		if c == 0 {
			break
		}
		buf = append(buf, c)
	}
	return syscall.UTF16ToString(buf), nil
}
