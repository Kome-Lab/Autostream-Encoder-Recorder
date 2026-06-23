//go:build windows

package streamproc

import (
	"path/filepath"
	"syscall"
	"unsafe"
)

func diskFreeBytes(path string) (uint64, bool) {
	dir := filepath.Dir(path)
	ptr, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, false
	}
	var freeBytes uint64
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("GetDiskFreeSpaceExW")
	result, _, _ := proc.Call(uintptr(unsafe.Pointer(ptr)), uintptr(unsafe.Pointer(&freeBytes)), 0, 0)
	if result == 0 {
		return 0, false
	}
	return freeBytes, true
}
