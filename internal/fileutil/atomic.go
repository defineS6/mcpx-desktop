// Package fileutil 提供同目录原子写入和进程内文件锁。
package fileutil

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

var fileLocks sync.Map

// WithFileLock 串行执行同一进程内对同一路径的操作；外部进程仍需由调用方检查文件快照。
func WithFileLock(path string, action func() error) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	key := filepath.Clean(absolute)
	if runtime.GOOS == "windows" {
		// Windows 路径大小写不敏感，同一文件的不同拼写必须共用一把锁。
		key = strings.ToLower(key)
	}
	value, _ := fileLocks.LoadOrStore(key, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	return action()
}

// AtomicWrite 在目标目录写入并同步临时文件后原子替换目标；替换失败不会删除原文件。
func AtomicWrite(path string, data []byte, mode os.FileMode) (err error) {
	file, err := os.CreateTemp(filepath.Dir(path), ".mcpx-write-*")
	if err != nil {
		return fmt.Errorf("创建临时文件 %s: %w", path, err)
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	defer file.Close()
	if err = file.Chmod(mode); err != nil {
		return fmt.Errorf("设置临时文件权限: %w", err)
	}
	if _, err = file.Write(data); err != nil {
		return fmt.Errorf("写入临时文件: %w", err)
	}
	if err = file.Sync(); err != nil {
		return fmt.Errorf("同步临时文件: %w", err)
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("关闭临时文件: %w", err)
	}
	if err = replaceFile(tempPath, path); err != nil {
		return fmt.Errorf("替换文件 %s: %w", path, err)
	}
	return nil
}
