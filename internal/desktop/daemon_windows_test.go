//go:build windows

package desktop

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/defineS6/mcpx-desktop/internal/config"
)

// isolatedDaemonConfig 将服务配置绑定临时目录及测试端口，不接触用户配置。
func isolatedDaemonConfig(t *testing.T, port int) {
	t.Helper()
	t.Setenv("MCPX_HOME", t.TempDir())
	t.Setenv("MCPX_RUNTIME", filepath.Join(t.TempDir(), "missing.exe"))
	cfg, err := config.LoadGlobal("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Server.Host, cfg.Server.Port = "127.0.0.1", port
	cfg.Auth.Mode, cfg.Auth.Token = "bearer", "fakeBearerToken"
	if err := config.WriteGlobal("", cfg); err != nil {
		t.Fatal(err)
	}
}

// TestDaemonPortConflict 验证第三方端口占用时拒绝启动且停止操作不会关闭占用者。
func TestDaemonPortConflict(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	isolatedDaemonConfig(t, listener.Addr().(*net.TCPAddr).Port)
	if state := currentState(); state.Status != statusConflict || state.PID != 0 || managed(state) {
		t.Fatalf("误认第三方服务：%+v", state)
	}
	if err := startService(); err == nil || !strings.Contains(err.Error(), "未受管") {
		t.Fatalf("冲突启动未正确拒绝：%v", err)
	}
	if err := stopService(); err != nil {
		t.Fatal(err)
	}
	if !portListening(listener.Addr().String()) {
		t.Fatal("停止操作影响第三方监听")
	}
	if err := waitForStatus(statusRunning, time.Second); err == nil {
		t.Fatal("端口冲突被误报为启动成功")
	}
	home, _ := config.HomeDir()
	if _, err := os.Stat(filepath.Join(home, daemonStateFilename)); !os.IsNotExist(err) {
		t.Fatalf("冲突仍创建状态文件：%v", err)
	}
}

// TestDaemonWaitTimeout 使用真实自身 PID 和空闲端口构造 starting，验证超时不是假成功。
func TestDaemonWaitTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	isolatedDaemonConfig(t, port)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(daemonState{PID: os.Getpid(), Executable: self})
	if err != nil {
		t.Fatal(err)
	}
	home, _ := config.HomeDir()
	if err := os.WriteFile(filepath.Join(home, daemonStateFilename), data, 0600); err != nil {
		t.Fatal(err)
	}
	if state := currentState(); state.Status != statusStarting {
		t.Fatalf("预期 starting：%+v", state)
	}
	started := time.Now()
	err = waitForStatus(statusRunning, 200*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("未真实报告超时：%v", err)
	}
	if time.Since(started) < 200*time.Millisecond {
		t.Fatal("未等待到超时")
	}
	// 此处只删除临时状态文件，绝不能对测试程序自身调用 stopService。
}

// TestDaemonStateIdentity 验证缺少 PID、完整路径、损坏记录均被拒绝，BOM 正常读取。
func TestDaemonStateIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), daemonStateFilename)
	for _, body := range []string{`{}`, `{"pid":1}`, `{"executable":"C:\\runtime.exe"}`, `{"pid":-1,"executable":"C:\\runtime.exe"}`, `{"pid":1,"executable":"runtime.exe"}`, `{"pid":1,"executable":""}`, `{`} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readDaemonState(path); err == nil {
			t.Errorf("接受无效身份：%s", body)
		}
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(daemonState{PID: os.Getpid(), Executable: self})
	if err := os.WriteFile(path, append([]byte{0xef, 0xbb, 0xbf}, data...), 0600); err != nil {
		t.Fatal(err)
	}
	state, err := readDaemonState(path)
	if err != nil || state.PID != os.Getpid() || state.Executable != self {
		t.Fatalf("BOM 身份读取失败：%+v %v", state, err)
	}
	if alive, matches := processAlive(os.Getpid(), ""); alive || matches {
		t.Fatal("空路径不应通过身份校验")
	}
}

// TestExternalRuntimeLifecycle 仅按显式选择的内核测试完整生命周期，所有状态和凭据均为临时测试数据。
func TestExternalRuntimeLifecycle(t *testing.T) {
	runtime := strings.TrimSpace(os.Getenv("MCPX_TEST_RUNTIME"))
	if runtime == "" {
		t.Skip("未设置 MCPX_TEST_RUNTIME，跳过真实内核生命周期")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	isolatedDaemonConfig(t, port)
	runtime, err = validateRuntimeFile(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveRuntimePath(runtime); err != nil {
		t.Fatal(err)
	}
	home, _ := config.HomeDir()
	statePath := filepath.Join(home, daemonStateFilename)
	owned := make(map[int]bool)
	// 清理必须先核对临时状态、完整路径及真实进程身份，禁止清理任何不相关进程。
	t.Cleanup(func() {
		persisted, err := readDaemonState(statePath)
		if os.IsNotExist(err) {
			return
		}
		if err != nil {
			t.Errorf("读取清理身份失败：%v", err)
			return
		}
		if !strings.EqualFold(persisted.Executable, runtime) {
			t.Errorf("清理路径不符，拒绝停止：%+v", persisted)
			return
		}
		alive, matches := processAlive(persisted.PID, runtime)
		if !alive {
			return
		}
		if !matches {
			t.Errorf("清理 PID 身份不符，拒绝停止：%d", persisted.PID)
			return
		}
		owned[persisted.PID] = true
		if err := stopService(); err != nil {
			t.Errorf("清理失败：%v", err)
		}
		for pid := range owned {
			if alive, _ := processAlive(pid, runtime); alive {
				t.Errorf("清理后进程仍存活：%d", pid)
			}
		}
	})
	if err := startService(); err != nil {
		t.Fatal(err)
	}
	first := currentState()
	if !first.running() || !strings.EqualFold(first.Executable, runtime) {
		t.Fatalf("启动身份不符：%+v", first)
	}
	if alive, matches := processAlive(first.PID, runtime); !alive || !matches {
		t.Fatal("启动 PID 路径校验失败")
	}
	owned[first.PID] = true
	payload := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"mcpx-isolated-test","version":"1"}}}`)
	request, err := http.NewRequest("POST", first.Endpoint, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer fakeBearerToken")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	_ = response.Body.Close()
	if readErr != nil || response.StatusCode != 200 || !bytes.Contains(body, []byte(`"protocolVersion"`)) || !bytes.Contains(body, []byte(`"result"`)) {
		t.Fatalf("MCP initialize 失败：%d %s %v", response.StatusCode, body, readErr)
	}
	if err := restartService(); err != nil {
		t.Fatal(err)
	}
	second := currentState()
	if !second.running() || !strings.EqualFold(second.Executable, runtime) {
		t.Fatalf("重启身份不符：%+v", second)
	}
	if alive, matches := processAlive(second.PID, runtime); !alive || !matches {
		t.Fatal("重启 PID 路径校验失败")
	}
	owned[second.PID] = true
	if err := stopService(); err != nil {
		t.Fatal(err)
	}
	if state := currentState(); state.Status != statusStopped {
		t.Fatalf("未完全停止：%+v", state)
	}
	for pid := range owned {
		if alive, _ := processAlive(pid, runtime); alive {
			t.Errorf("停止后进程残留：%d", pid)
		}
	}
}
