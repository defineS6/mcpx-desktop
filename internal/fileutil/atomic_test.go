package fileutil

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestAtomicWrite 验证新建、替换和失败时保留原文件，且不遗留临时文件。
func TestAtomicWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	path := filepath.Join(home, "config.yaml")
	for _, data := range []string{"first", "second"} {
		if err := AtomicWrite(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != data {
			t.Fatalf("原子替换：%q, %v", got, err)
		}
	}
	badPath := filepath.Join(home, "directory")
	if err := os.Mkdir(badPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(badPath, []byte("failure"), 0o600); err == nil {
		t.Fatal("替换目录应失败")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "second" {
		t.Fatalf("失败后文件损坏：%q, %v", got, err)
	}
	files, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasPrefix(file.Name(), ".mcpx-write-") {
			t.Fatalf("遗留临时文件：%s", file.Name())
		}
	}
}

// TestWindowsFileLockCase 验证 Windows 上大小写不同的同一路径共用一把锁。
func TestWindowsFileLockCase(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅 Windows 路径大小写不敏感")
	}
	path := filepath.Join(t.TempDir(), "lockfile")
	for _, candidate := range []string{path, filepath.Join(filepath.Dir(path), "LOCKFILE")} {
		if err := WithFileLock(candidate, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	fileLocks.Range(func(key, _ any) bool {
		if strings.EqualFold(key.(string), path) {
			count++
		}
		return true
	})
	if count != 1 {
		t.Fatalf("同一路径生成了 %d 把锁", count)
	}
}

// TestWithFileLock 验证进程内操作可串行运行。
func TestWithFileLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	count := 0
	path := filepath.Join(home, "lock")
	finished := make(chan error, 8)
	for range 8 {
		go func() { finished <- WithFileLock(path, func() error { count++; return nil }) }()
	}
	for range 8 {
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
	}
	if count != 8 {
		t.Fatalf("计数：%d", count)
	}
}
