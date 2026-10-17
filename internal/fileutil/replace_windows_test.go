//go:build windows

package fileutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestAtomicWriteWindowsBusy 使用真实句柄验证占用错误、释放后的恢复及持续占用时的数据安全。
func TestAtomicWriteWindowsBusy(t *testing.T) {
	for _, share := range []uint32{0, windows.FILE_SHARE_READ} {
		for _, release := range []bool{true, false} {
			t.Run(fmt.Sprintf("share=%d/release=%t", share, release), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "target.json")
				if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
					t.Fatal(err)
				}
				name, err := windows.UTF16PtrFromString(path)
				if err != nil {
					t.Fatal(err)
				}
				handle, err := windows.CreateFile(name, windows.GENERIC_READ, share, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if handle != windows.InvalidHandle {
						_ = windows.CloseHandle(handle)
					}
				}()

				// 直接调用系统 API，先证明当前句柄确实阻止替换，而非假定占用原因。
				probe := filepath.Join(dir, "probe")
				if err := os.WriteFile(probe, []byte("probe"), 0o600); err != nil {
					t.Fatal(err)
				}
				from, err := windows.UTF16PtrFromString(probe)
				if err != nil {
					t.Fatal(err)
				}
				err = windows.MoveFileEx(from, name, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
				if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
					t.Fatalf("句柄未产生预期占用错误：%v", err)
				}
				t.Logf("受控句柄下直接 MoveFileEx：%v", err)
				if err := os.Remove(probe); err != nil {
					t.Fatal(err)
				}

				started := time.Now()
				done := make(chan error, 1)
				go func() { done <- AtomicWrite(path, []byte("new"), 0o600) }()
				if release {
					select {
					case err := <-done:
						t.Fatalf("释放前已结束：%v", err)
					case <-time.After(60 * time.Millisecond):
					}
					if err := windows.CloseHandle(handle); err != nil {
						t.Fatal(err)
					}
					handle = windows.InvalidHandle
				}
				select {
				case err = <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("原子替换未在有界窗口内结束")
				}
				elapsed := time.Since(started)
				if elapsed > time.Second {
					t.Fatalf("替换超过一秒：%v", elapsed)
				}
				want := "new"
				if release {
					if err != nil {
						t.Fatalf("释放后未恢复：%v", err)
					}
				} else {
					if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
						t.Fatalf("未保留占用错误：%v", err)
					}
					if elapsed < 400*time.Millisecond {
						t.Fatalf("未执行重试：%v", elapsed)
					}
					if err := windows.CloseHandle(handle); err != nil {
						t.Fatal(err)
					}
					handle = windows.InvalidHandle
					want = "old"
				}
				data, err := os.ReadFile(path)
				if err != nil || string(data) != want {
					t.Fatalf("目标内容：%q，期望 %q，错误 %v", data, want, err)
				}
				entries, err := os.ReadDir(dir)
				if err != nil || len(entries) != 1 || entries[0].Name() != "target.json" {
					t.Fatalf("存在残留文件：%v，%v", entries, err)
				}
			})
		}
	}
}

// TestWindowsReplaceErrorClassification 验证白名单内错误可恢复，其他错误只尝试一次且保留错误链。
func TestWindowsReplaceErrorClassification(t *testing.T) {
	for _, code := range []error{
		windows.ERROR_SHARING_VIOLATION, windows.ERROR_ACCESS_DENIED, windows.ERROR_LOCK_VIOLATION,
		windows.ERROR_PATH_NOT_FOUND, windows.ERROR_DISK_FULL, windows.ERROR_INVALID_PARAMETER,
	} {
		t.Run(code.Error(), func(t *testing.T) {
			calls := 0
			wrapped := fmt.Errorf("系统替换失败：%w", code)
			got := retryWindowsReplace(func() error {
				calls++
				if calls == 1 {
					return wrapped
				}
				return nil
			})
			retryable := errors.Is(code, windows.ERROR_SHARING_VIOLATION) || errors.Is(code, windows.ERROR_ACCESS_DENIED) || errors.Is(code, windows.ERROR_LOCK_VIOLATION)
			if retryable {
				if got != nil || calls != 2 {
					t.Fatalf("未恢复：calls=%d err=%v", calls, got)
				}
			} else if got != wrapped || calls != 1 {
				t.Fatalf("非占用错误被重试或丢失：calls=%d err=%v", calls, got)
			}
		})
	}
}

// TestAtomicWriteWindowsReadOnly 验证真实拒绝写入不会被吞掉，且保留旧文件并清理临时文件。
func TestAtomicWriteWindowsReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "readonly.json")
	if err := os.WriteFile(path, []byte("old"), 0o400); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0o600)
	started := time.Now()
	err := AtomicWrite(path, []byte("new"), 0o600)
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("未保留真实权限错误：%v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("权限错误重试超过一秒：%v", elapsed)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "old" {
		t.Fatalf("原文件损坏：%q %v", data, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("遗留临时文件：%v %v", entries, err)
	}
}

// TestWindowsFileLockSerializesAliases 验证同一路径的不同大小写不能并发进入临界区。
func TestWindowsFileLockSerializesAliases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	entered := make(chan struct{})
	release := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- WithFileLock(path, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	second := make(chan error, 1)
	go func() { second <- WithFileLock(strings.ToUpper(path), func() error { return nil }) }()
	select {
	case err := <-second:
		close(release)
		<-first
		t.Fatalf("同一路径未被锁住：%v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	for _, result := range []chan error{first, second} {
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("释放后路径锁未恢复")
		}
	}
}
