//go:build windows

package winproc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// TestStateFullProcessIdentity 使用真实自身进程验证完整路径，而不是只比对程序名。
func TestStateFullProcessIdentity(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{self, strings.ToUpper(self)} {
		alive, matches, err := State(os.Getpid(), path)
		if err != nil || !alive || !matches {
			t.Fatalf("真实自身身份未匹配：%q %v %v %v", path, alive, matches, err)
		}
	}
	other := filepath.Join(t.TempDir(), filepath.Base(self))
	if err := os.WriteFile(other, []byte("不同目录同名程序"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{other, "", filepath.Base(self)} {
		alive, matches, err := State(os.Getpid(), path)
		if err != nil || !alive || matches {
			t.Fatalf("错误路径被认作自身：%q %v %v %v", path, alive, matches, err)
		}
	}
	for _, pid := range []int{0, -1} {
		alive, matches, err := State(pid, self)
		if err != nil || alive || matches {
			t.Fatalf("无效 PID 被接受：%d %v %v %v", pid, alive, matches, err)
		}
	}
}

// TestEqualFileNameRejectsBasename 验证不同目录同名文件和空路径不能通过纯路径校验。
func TestEqualFileNameRejectsBasename(t *testing.T) {
	a := filepath.Join(t.TempDir(), "mcpx.exe")
	b := filepath.Join(t.TempDir(), "mcpx.exe")
	for _, path := range []string{a, b} {
		if err := os.WriteFile(path, []byte("test"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if equalFileName(a, b) || equalFileName(a, "") || equalFileName(a, "mcpx.exe") {
		t.Fatal("同名或空路径被误判为同一身份")
	}
	if !equalFileName(a, strings.ToUpper(a)) {
		t.Fatal("Windows 路径大小写未正确处理")
	}
}

// TestConfigureNoWindow 验证无窗口标志生效、冲突标志移除且其他进程属性保留。
func TestConfigureNoWindow(t *testing.T) {
	ConfigureNoWindow(nil)
	command := exec.Command("not-executed.exe")
	ConfigureNoWindow(command)
	if command.SysProcAttr == nil || !command.SysProcAttr.HideWindow || command.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatal("未设置无窗口属性")
	}
	attrs := &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_CONSOLE | windows.CREATE_NEW_PROCESS_GROUP, CmdLine: "保留命令行"}
	command.SysProcAttr = attrs
	ConfigureNoWindow(command)
	ConfigureNoWindow(command)
	if command.SysProcAttr != attrs || !attrs.HideWindow || attrs.CreationFlags != windows.CREATE_NO_WINDOW|windows.CREATE_NEW_PROCESS_GROUP || attrs.CmdLine != "保留命令行" {
		t.Fatalf("进程属性不符：%+v", attrs)
	}
}
