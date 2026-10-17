//go:build windows

package fileutil

import (
	"errors"
	"time"

	"golang.org/x/sys/windows"
)

// replaceFile 使用 Windows 写透标记替换已有文件。
func replaceFile(source, destination string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return retryWindowsReplace(func() error {
		return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
	})
}

// retryWindowsReplace 仅为可能由句柄占用导致的错误提供最多 500 毫秒的重试窗口。
// ACCESS_DENIED 也可能是真实权限错误，不能据此认定是占用；超时原样返回，绝不删除目标。
func retryWindowsReplace(move func() error) error {
	deadline := time.Now().Add(500 * time.Millisecond)
	delay := 5 * time.Millisecond
	for {
		err := move()
		if err == nil || (!errors.Is(err, windows.ERROR_SHARING_VIOLATION) &&
			!errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_LOCK_VIOLATION)) {
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return err
		}
		if delay > remaining {
			delay = remaining
		}
		time.Sleep(delay)
		// 调度延迟也计入窗口，超时不再发起新的替换。
		if !time.Now().Before(deadline) {
			return err
		}
		if delay < 50*time.Millisecond {
			delay *= 2
			if delay > 50*time.Millisecond {
				delay = 50 * time.Millisecond
			}
		}
	}
}
