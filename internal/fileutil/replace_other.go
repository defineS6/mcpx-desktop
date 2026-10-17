//go:build !windows

package fileutil

import "os"

// replaceFile 在非 Windows 系统使用同目录重命名替换。
func replaceFile(source, destination string) error { return os.Rename(source, destination) }
