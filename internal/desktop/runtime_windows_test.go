//go:build windows

package desktop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// init 仅在显式标记的版本探测子进程中模拟内核，不启动测试或真实服务。
func init() {
	if os.Getenv("MCPX_DESKTOP_TEST_VERSION_HELPER") != "1" || len(os.Args) != 2 || os.Args[1] != "-version" {
		return
	}
	if path := os.Getenv("MCPX_DESKTOP_TEST_VERSION_COUNT"); path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(2)
		}
		_, _ = f.WriteString("probe\n")
		_ = f.Close()
	}
	if os.Getenv("MCPX_DESKTOP_TEST_VERSION_SLEEP") == "1" {
		time.Sleep(30 * time.Second)
	}
	fmt.Println("mcpx test-isolated")
	os.Exit(0)
}

// isolatedRuntimeHelper 复制当前测试程序作为独立内核替身，所有读写限于临时目录。
func isolatedRuntimeHelper(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "mcpx.exe")
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCPX_DESKTOP_TEST_VERSION_HELPER", "1")
	return path
}

// TestRuntimeFileValidation 验证自身、缺失、目录、非 exe 均被拒绝。
func TestRuntimeFileValidation(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	text := filepath.Join(dir, "mcpx.txt")
	if err := os.WriteFile(text, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	exeDir := filepath.Join(dir, "directory.exe")
	if err := os.Mkdir(exeDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{self, filepath.Join(dir, "missing.exe"), exeDir, text} {
		if _, err := validateRuntimeFile(path); err == nil {
			t.Errorf("不应接受 %s", path)
		}
	}
	valid := filepath.Join(dir, "runtime.EXE")
	if err := os.WriteFile(valid, []byte("仅验证文件类型"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateRuntimeFile(valid); err != nil {
		t.Fatal(err)
	}
}

// TestRuntimeSettings 验证 BOM、未知字段保留与损坏设置拒绝覆盖。
func TestRuntimeSettings(t *testing.T) {
	t.Setenv("MCPX_HOME", t.TempDir())
	path, err := managerConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	original := []byte("\xef\xbb\xbf" + `{"runtime_path":" old.exe ","future":{"enabled":true,"items":[1,2]}}`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := configuredRuntimePath(); err != nil || got != "old.exe" {
		t.Fatalf("读取 BOM：%q %v", got, err)
	}
	if err := saveRuntimePath(""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}) {
		t.Fatal("保存不应生成 BOM")
	}
	var saved map[string]json.RawMessage
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	var future struct {
		Enabled bool
		Items   []int
	}
	if err := json.Unmarshal(saved["future"], &future); err != nil || !future.Enabled || len(future.Items) != 2 {
		t.Fatalf("未知字段丢失：%s", data)
	}
	for _, broken := range []string{`{"runtime_path":`, `null`, `[]`} {
		if err := os.WriteFile(path, []byte(broken), 0600); err != nil {
			t.Fatal(err)
		}
		if err := saveRuntimePath(""); err == nil {
			t.Errorf("接受损坏设置 %s", broken)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != broken {
			t.Fatalf("损坏设置被覆盖：%s %v", got, err)
		}
	}
}

// TestRuntimeEnvironmentSelection 验证环境变量选择与显式设置优先级，不搜索本机内核。
func TestRuntimeEnvironmentSelection(t *testing.T) {
	t.Setenv("MCPX_HOME", t.TempDir())
	helper := isolatedRuntimeHelper(t)
	t.Setenv("MCPX_RUNTIME", helper)
	got, err := resolveRuntimePath()
	if err != nil || got != helper {
		t.Fatalf("环境选择：%q %v", got, err)
	}
	if err := saveRuntimePath(helper); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCPX_RUNTIME", filepath.Join(t.TempDir(), "missing.exe"))
	got, err = resolveRuntimePath()
	if err != nil || got != helper {
		t.Fatalf("设置优先：%q %v", got, err)
	}
	if err := saveRuntimePath(""); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveRuntimePath(); err == nil {
		t.Fatal("显式无效环境路径不应回退到本机内核")
	}
}

// TestRuntimeVersionTimeoutCache 用真实子进程验证超时失败和失败缓存，避免轮询反复启动。
func TestRuntimeVersionTimeoutCache(t *testing.T) {
	t.Setenv("MCPX_HOME", t.TempDir())
	helper := isolatedRuntimeHelper(t)
	count := filepath.Join(t.TempDir(), "count")
	t.Setenv("MCPX_DESKTOP_TEST_VERSION_COUNT", count)
	t.Setenv("MCPX_DESKTOP_TEST_VERSION_SLEEP", "1")
	started := time.Now()
	_, first := inspectRuntimeVersion(helper)
	if first == nil {
		t.Fatal("慢速版本探测未超时")
	}
	if elapsed := time.Since(started); elapsed < 4*time.Second || elapsed > 12*time.Second {
		t.Fatalf("探测耗时异常：%s", elapsed)
	}
	started = time.Now()
	_, second := inspectRuntimeVersion(helper)
	if second == nil || second.Error() != first.Error() {
		t.Fatalf("失败未缓存：%v / %v", first, second)
	}
	if time.Since(started) > time.Second {
		t.Fatal("缓存命中仍等待子进程")
	}
	data, err := os.ReadFile(count)
	if err != nil || string(data) != "probe\n" {
		t.Fatalf("探测次数异常：%q %v", data, err)
	}
}
