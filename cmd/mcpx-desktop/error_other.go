//go:build !windows

package main

import (
	"fmt"
	"os"
)

// showError 在不支持图形界面的平台通过标准错误说明原因。
func showError(message string) { fmt.Fprintln(os.Stderr, message) }
