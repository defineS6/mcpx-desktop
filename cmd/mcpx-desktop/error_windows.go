//go:build windows

package main

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// showError 在无控制台的发行程序中显示启动错误，避免用户双击后无响应。
func showError(message string) {
	fmt.Fprintln(os.Stderr, message)
	text, _ := windows.UTF16PtrFromString(message)
	title, _ := windows.UTF16PtrFromString("MCPX Desktop 启动失败")
	windows.NewLazySystemDLL("user32.dll").NewProc("MessageBoxW").Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), 0x10)
}
