//go:build windows

package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/defineS6/mcpx-desktop/internal/config"
	"github.com/defineS6/mcpx-desktop/internal/winproc"
	"golang.org/x/sys/windows"
)

// TestCloudflareJSON 验证 BOM、未知字段、损坏文件保护与 PID 定向清理。
func TestCloudflareJSON(t *testing.T) {
	t.Setenv("MCPX_HOME", t.TempDir())
	_, cfgPath, statePath, _, _, err := cloudflarePaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("\xef\xbb\xbf"+`{"mode":"named","other":{"v":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCloudflareDesktopConfig()
	if err != nil || cfg.Mode != "named" {
		t.Fatalf("读取 BOM 配置失败：%+v %v", cfg, err)
	}
	if err := setCloudflareDesiredRunning(true); err != nil {
		t.Fatal(err)
	}
	if err := setCloudflareLastPublicURL("https://new.trycloudflare.com"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(data), `"other"`) || !strings.Contains(string(data), `"desired_running": true`) || !strings.Contains(string(data), `"last_public_url": "https://new.trycloudflare.com"`) {
		t.Fatalf("配置字段丢失：%s", data)
	}
	if err := os.WriteFile(statePath, []byte("\xef\xbb\xbf"+`{"pid":123,"future":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := readCloudflareProcessState()
	if err != nil || state.PID != 123 {
		t.Fatalf("读取 BOM 状态失败：%+v %v", state, err)
	}
	if err := setCloudflareProcessURLIfPID(123, "https://example.com"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(statePath)
	if !strings.Contains(string(data), `"future"`) {
		t.Fatalf("状态未知字段丢失：%s", data)
	}
	clearCloudflareProcessStateIfPID(456)
	if _, err := os.Stat(statePath); err != nil {
		t.Fatal(err)
	}
	clearCloudflareProcessStateIfPID(123)
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("PID 匹配后未删除：%v", err)
	}
	for _, path := range []string{cfgPath, statePath} {
		broken := []byte(`{"broken":`)
		if err := os.WriteFile(path, broken, 0o600); err != nil {
			t.Fatal(err)
		}
		if path == cfgPath {
			_, err = saveCloudflareDesktopConfig(defaultCloudflareDesktopConfig())
		} else {
			err = writeCloudflareProcessState(cloudflareProcessState{PID: 789})
		}
		if err == nil {
			t.Fatalf("损坏文件 %s 被覆盖", path)
		}
		data, _ := os.ReadFile(path)
		if string(data) != string(broken) {
			t.Fatalf("损坏文件 %s 内容变化", path)
		}
	}
}

// TestCloudflareTailSegments 验证尾随仅读取新增字节并拼接跨写入段的 URL。
func TestCloudflareTailSegments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cloudflared.log")
	if err := os.WriteFile(path, []byte("https://old.trycloudflare.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, _ := file.Stat()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exited := make(chan struct{})
	lines := make(chan string, 4)
	go tailCloudflaredLog(ctx, path, info.Size(), exited, lines)
	if _, err := file.WriteString("https://new.trycloud"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	select {
	case line := <-lines:
		t.Fatalf("未换行片段提前输出：%q", line)
	default:
	}
	if _, err := file.WriteString("flare.com\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-lines:
		if line != "https://new.trycloudflare.com" {
			t.Fatalf("URL 跨分段拼接失败：%q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未收到新日志")
	}
	cancel()
	select {
	case <-lines:
	case <-time.After(time.Second):
		t.Fatal("尾随取消后未关闭通道")
	}
}

// TestCloudflareChildProcess 作为隔离测试子进程，分段写日志后保持存活。
func TestCloudflareChildProcess(t *testing.T) {
	if os.Getenv("MCPX_CLOUDFLARE_TEST_CHILD") != "1" {
		return
	}
	switch os.Getenv("MCPX_CLOUDFLARE_TEST_BEHAVIOR") {
	case "exit":
		return
	case "silent":
		time.Sleep(20 * time.Second)
		return
	}
	_, _ = os.Stdout.WriteString("https://child.trycloud")
	time.Sleep(250 * time.Millisecond)
	_, _ = os.Stderr.WriteString("flare.com\n")
	time.Sleep(500 * time.Millisecond)
	_, _ = os.Stdout.WriteString("child still alive\n")
	time.Sleep(20 * time.Second)
}

// TestCloudflareSpawn 验证真实测试子进程的句柄、异步回收及 Quick 增量日志。
func TestCloudflareSpawn(t *testing.T) {
	t.Setenv("MCPX_HOME", t.TempDir())
	t.Setenv("MCPX_CLOUDFLARE_TEST_CHILD", "1")
	t.Setenv("MCPX_CLOUDFLARE_TEST_BEHAVIOR", "")
	for _, mode := range []string{"quick", "named"} {
		t.Run(mode, func(t *testing.T) {
			cmd, lines, exited, cancel, err := spawnCloudflared(os.Args[0], []string{"-test.run=^TestCloudflareChildProcess$"}, "", cloudflareProcessState{Mode: mode})
			if err != nil {
				t.Fatal(err)
			}
			defer cancel()
			defer func() {
				_ = cmd.Process.Kill()
				select {
				case <-exited:
				case <-time.After(3 * time.Second):
					t.Error("子进程未回收")
				}
			}()
			if mode == "quick" {
				origin, err := waitForTryCloudflareURL(cmd, lines, exited, 3*time.Second)
				if err != nil || origin != "https://child.trycloudflare.com" {
					t.Fatalf("未获取跨标准输出的公网地址：%q %v", origin, err)
				}
			} else if lines != nil {
				t.Fatal("Named 模式不应启动日志尾随")
			}
			if _, ok := cmd.Stdout.(*os.File); !ok || cmd.Stderr != cmd.Stdout {
				t.Fatal("子进程未直接继承日志文件句柄")
			}
			if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
				t.Fatal("子进程未配置隐藏窗口")
			}
			state, err := readCloudflareProcessState()
			if err != nil || !filepath.IsAbs(state.Executable) {
				t.Fatalf("未记录完整进程路径：%+v %v", state, err)
			}
		})
	}
}

// TestCloudflareOriginAndRecovery 验证 Origin 边界及公网配置 4xx 不触发重启。
func TestCloudflareOriginAndRecovery(t *testing.T) {
	for _, raw := range []string{"http://example.com", "https://user:pass@example.com", "https://example.com/mcp", "https://example.com?", "https://example.com/#frag", "https://example.com#", "https://example.com:70000", "https://[::1]:", "https://[::1]/mcp"} {
		if _, err := normalizePublicOrigin(raw); err == nil {
			t.Errorf("错误接受非 Origin：%s", raw)
		}
	}
	for _, raw := range []string{"https://example.com/", "https://[::1]:8443/"} {
		if _, err := normalizePublicOrigin(raw); err != nil {
			t.Errorf("合法 Origin %s：%v", raw, err)
		}
	}
	for _, item := range []cloudflareHealthItem{{StatusCode: 403}, {StatusCode: 404}} {
		if cloudflareHealthNeedsAutoRecovery(cloudflareHealth{LocalMCP: cloudflareHealthItem{OK: true}, PublicMCP: item, OAuthMetadata: cloudflareHealthItem{OK: true}}) {
			t.Fatal("公网 4xx 不应自动恢复")
		}
	}
	if !cloudflareHealthNeedsAutoRecovery(cloudflareHealth{LocalMCP: cloudflareHealthItem{OK: false}}) {
		t.Fatal("本地 HTTP 不健康应允许恢复")
	}
}

type cloudflareTestTransport struct{ server *httptest.Server }

// RoundTrip 将固定官方 URL 映射到测试服务，禁止测试访问真实 GitHub。
func (transport cloudflareTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(transport.server.URL, "http://")
	clone.Host = clone.URL.Host
	return http.DefaultTransport.RoundTrip(clone)
}

// TestCloudflareVerifiedDownload 验证元数据大小限制、摘要缺失及错误摘要不替换旧文件。
func TestCloudflareVerifiedDownload(t *testing.T) {
	asset := "cloudflared-windows-amd64.exe"
	body := []byte("verified executable")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	var releaseDigest = "sha256:" + digest
	var releaseBody string
	var oversized bool
	var mu sync.Mutex
	setRelease := func(digest, body string, large bool) {
		mu.Lock()
		defer mu.Unlock()
		releaseDigest, releaseBody, oversized = digest, body, large
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(r.URL.Path, "/repos/") {
			if oversized {
				_, _ = io.WriteString(w, strings.Repeat(" ", int(cloudflareMaxMetadataBytes)+1))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"body": releaseBody, "assets": []map[string]any{{"name": asset, "size": len(body), "digest": releaseDigest, "browser_download_url": "https://github.com/cloudflare/cloudflared/releases/download/2026.1.1/" + asset}}})
			return
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()
	client := &http.Client{Transport: cloudflareTestTransport{server: server}, Timeout: 2 * time.Second}
	destination := filepath.Join(t.TempDir(), asset)
	if err := os.WriteFile(destination, []byte("old binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	url := "https://api.github.com/repos/cloudflare/cloudflared/releases/latest"
	for _, bad := range []string{"", "sha256:" + strings.Repeat("0", 64)} {
		setRelease(bad, "", false)
		if err := downloadVerifiedCloudflared(client, url, asset, destination); err == nil {
			t.Fatalf("摘要 %q 应拒绝", bad)
		}
		data, _ := os.ReadFile(destination)
		if string(data) != "old binary" {
			t.Fatal("失败时覆盖了旧文件")
		}
	}
	setRelease("", "", true)
	if err := downloadVerifiedCloudflared(client, url, asset, destination); err == nil {
		t.Fatal("超过 4MB 的元数据未被拒绝")
	}
	setRelease("", asset+": SHA256 "+digest, false)
	if err := downloadVerifiedCloudflared(client, url, asset, destination); err != nil {
		t.Fatal(err)
	}
	setRelease("sha256:"+digest, "", false)
	if err := downloadVerifiedCloudflared(client, url, asset, destination); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(destination)
	if err != nil || string(data) != string(body) {
		t.Fatalf("安装文件不一致：%v", err)
	}
}

// TestCloudflareConcurrentFiles 验证配置和状态锁互不嵌套，读者始终看到完整 JSON。
func TestCloudflareConcurrentFiles(t *testing.T) {
	t.Setenv("MCPX_HOME", t.TempDir())
	if _, err := saveCloudflareDesktopConfig(defaultCloudflareDesktopConfig()); err != nil {
		t.Fatal(err)
	}
	if err := writeCloudflareProcessState(cloudflareProcessState{PID: 123}); err != nil {
		t.Fatal(err)
	}
	_, cfgPath, statePath, _, _, _ := cloudflarePaths()
	errors := make(chan error, 100)
	var workers sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := 0; i < 20; i++ {
				for _, err := range []error{
					setCloudflareDesiredRunning(i%2 == 0),
					setCloudflareLastPublicURL("https://concurrent.trycloudflare.com"),
					writeCloudflareProcessState(cloudflareProcessState{PID: 123}),
					setCloudflareProcessURLIfPID(123, "https://example.com"),
				} {
					if err != nil {
						errors <- err
						return
					}
				}
				clearCloudflareProcessStateIfPID(456)
				for _, path := range []string{cfgPath, statePath} {
					if _, err := readCloudflareJSON(path); err != nil {
						errors <- err
						return
					}
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("配置或状态锁发生死锁")
	}
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(cfgPath), ".mcpx-write-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("原子写残留临时文件：%v %v", files, err)
	}
}

// TestCloudflareManagerProcess 模拟退出管理器，子进程只能继承日志文件而不能依赖父管道。
func TestCloudflareManagerProcess(t *testing.T) {
	if os.Getenv("MCPX_CLOUDFLARE_TEST_MANAGER") != "1" {
		return
	}
	_, _, _, cancel, err := spawnCloudflared(os.Args[0], []string{"-test.run=^TestCloudflareChildProcess$"}, "", cloudflareProcessState{Mode: "named"})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	os.Exit(0)
}

// TestCloudflareChildSurvivesManager 验证管理器实际退出后子进程仍可运行并继续写日志。
func TestCloudflareChildSurvivesManager(t *testing.T) {
	t.Setenv("MCPX_HOME", t.TempDir())
	t.Setenv("MCPX_CLOUDFLARE_TEST_CHILD", "1")
	t.Setenv("MCPX_CLOUDFLARE_TEST_MANAGER", "1")
	t.Setenv("MCPX_CLOUDFLARE_TEST_BEHAVIOR", "")
	command := exec.Command(os.Args[0], "-test.run=^TestCloudflareManagerProcess$")
	winproc.ConfigureNoWindow(command)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("管理器退出失败：%v %s", err, output)
	}
	state, err := readCloudflareProcessState()
	if err != nil {
		t.Fatal(err)
	}
	process, err := os.FindProcess(state.PID)
	if err != nil {
		t.Fatal(err)
	}
	defer process.Release()
	defer func() {
		_ = process.Kill()
		if !waitDesktopProcessGone(state.PID, state.Executable, 3*time.Second) {
			t.Error("孤立测试子进程清理超时")
		}
	}()
	_, _, _, logPath, _, _ := cloudflarePaths()
	deadline := time.Now().Add(4 * time.Second)
	for {
		data, err := os.ReadFile(logPath)
		if err == nil && strings.Contains(string(data), "child still alive") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("管理器退出后子进程未继续写日志：%v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if alive, matches := processAlive(state.PID, state.Executable); !alive || !matches {
		t.Fatal("管理器退出后子进程未保持存活")
	}
}

// TestCloudflareWaitFailure 验证命令提前退出与无 URL 超时后可回收进程、状态及日志尾随。
func TestCloudflareWaitFailure(t *testing.T) {
	t.Setenv("MCPX_HOME", t.TempDir())
	t.Setenv("MCPX_CLOUDFLARE_TEST_CHILD", "1")
	for _, behavior := range []string{"exit", "silent"} {
		t.Run(behavior, func(t *testing.T) {
			t.Setenv("MCPX_CLOUDFLARE_TEST_BEHAVIOR", behavior)
			cmd, lines, exited, cancel, err := spawnCloudflared(os.Args[0], []string{"-test.run=^TestCloudflareChildProcess$"}, "", cloudflareProcessState{Mode: "quick"})
			if err != nil {
				t.Fatal(err)
			}
			defer cancel()
			defer cmd.Process.Kill()
			timeout := 400 * time.Millisecond
			if behavior == "exit" {
				timeout = 3 * time.Second
			}
			_, err = waitForTryCloudflareURL(cmd, lines, exited, timeout)
			if err == nil || (behavior == "silent" && !strings.Contains(err.Error(), "没有返回")) || (behavior == "exit" && !strings.Contains(err.Error(), "退出")) {
				t.Fatalf("失败原因不符合预期：%v", err)
			}
			_ = cmd.Process.Kill()
			select {
			case <-exited:
			case <-time.After(3 * time.Second):
				t.Fatal("失败进程未被回收")
			}
			cancel()
			for range lines {
			}
			if _, err := readCloudflareProcessState(); !os.IsNotExist(err) {
				t.Fatalf("失败进程状态未清除：%v", err)
			}
		})
	}
}

// TestCloudflareConfigModes 验证 Quick/Named 配置、认证边界及受管 YAML 无损联动。
func TestCloudflareConfigModes(t *testing.T) {
	t.Setenv("MCPX_HOME", t.TempDir())
	cfg, err := loadCloudflareDesktopConfig()
	if err != nil || cfg.Mode != "quick" || cfg.ManageWithMCPX || !cfg.AutoRecover || !cfg.SyncOAuthServerURL {
		t.Fatalf("默认 Quick 配置异常：%+v %v", cfg, err)
	}
	runtime := config.DefaultConfig()
	runtime.Auth.Mode = "bearer"
	if err := validateCloudflareStartConfig(cfg, runtime); err != nil {
		t.Fatal(err)
	}
	cfg.Mode = "named"
	if err := validateCloudflareStartConfig(cfg, runtime); err == nil {
		t.Fatal("Named 未配置凭据却允许启动")
	}
	cfg.TunnelToken = "test-only-token"
	if err := validateCloudflareStartConfig(cfg, runtime); err == nil {
		t.Fatal("Named 未配置公网地址却允许启动")
	}
	cfg.PublicURL = " https://mcp.example.com/ "
	cfg, err = saveCloudflareDesktopConfig(cfg)
	if err != nil || cfg.PublicURL != "https://mcp.example.com" {
		t.Fatalf("Named 配置规范化失败：%+v %v", cfg, err)
	}
	if err := validateCloudflareStartConfig(cfg, runtime); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"open", "invalid"} {
		runtime.Auth.Mode = mode
		if err := validateCloudflareStartConfig(cfg, runtime); err == nil {
			t.Errorf("不安全鉴权模式 %s 未被拒绝", mode)
		}
	}
	runtime.Auth.Mode = "bearer"
	cfg.Mode = "invalid"
	if err := validateCloudflareStartConfig(cfg, runtime); err == nil {
		t.Fatal("无效 Tunnel 模式未被拒绝")
	}
	path, err := config.GlobalConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	original := "server:\n  host: 127.0.0.1\n  port: 9090\nauth:\n  mode: bearer\n  token: test-only-token\nfuture:\n  enabled: true\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := ensureCloudflareRuntimeConfig("https://mcp.example.com", true)
	if err != nil || !changed {
		t.Fatalf("首次联动未写入：%v %v", changed, err)
	}
	updated, err := config.LoadGlobal(path)
	if err != nil || !updated.Server.DisableLocalhostProtection || !updated.Server.TrustProxyHeaders || updated.Auth.OAuth.ServerURL != "https://mcp.example.com" {
		t.Fatalf("运行配置联动异常：%+v %v", updated.Server, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "future:") || !strings.Contains(string(data), "enabled: true") {
		t.Fatalf("联动破坏未知 YAML 字段：%v", err)
	}
	if changed, err := ensureCloudflareRuntimeConfig("https://mcp.example.com", true); err != nil || changed {
		t.Fatalf("重复联动不应改写：%v %v", changed, err)
	}
}

// TestCloudflareDownloadFailureSafety 验证 HTTP、长度、摘要及替换失败均保留旧文件且清理临时文件。
func TestCloudflareDownloadFailureSafety(t *testing.T) {
	body := []byte("test binary bytes")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/forbidden" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()
	for _, scenario := range []struct {
		name, path, digest string
		size               int64
		locked             bool
	}{
		{name: "HTTP拒绝", path: "/forbidden", digest: digest, size: int64(len(body))},
		{name: "长度不符", digest: digest, size: int64(len(body) + 1)},
		{name: "摘要不符", digest: strings.Repeat("0", 64), size: int64(len(body))},
		{name: "目标占用", digest: digest, size: int64(len(body)), locked: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			dir := t.TempDir()
			destination := filepath.Join(dir, "cloudflared.exe")
			if err := os.WriteFile(destination, []byte("old binary"), 0o600); err != nil {
				t.Fatal(err)
			}
			if scenario.locked {
				path, err := windows.UTF16PtrFromString(destination)
				if err != nil {
					t.Fatal(err)
				}
				handle, err := windows.CreateFile(path, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer windows.CloseHandle(handle)
			}
			if err := fetchVerifiedCloudflared(server.Client(), server.URL+scenario.path, destination, scenario.digest, scenario.size); err == nil {
				t.Fatal("应拒绝替换但返回成功")
			}
			data, err := os.ReadFile(destination)
			if err != nil || string(data) != "old binary" {
				t.Fatalf("失败时破坏旧文件：%v", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("下载失败残留临时文件：%v %v", entries, err)
			}
		})
	}
}

// TestCloudflareHealthLayers 验证本地鉴权响应可达、公网配置错误与临时故障的恢复分层。
func TestCloudflareHealthLayers(t *testing.T) {
	for _, status := range []int{200, 202, 204, 400, 401, 403, 404, 405, 406, 502} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer server.Close()
			local := checkLocalMCPHealth(serviceState{Status: statusRunning, Endpoint: server.URL})
			want := status == 200 || status == 202 || status == 204 || status == 400 || status == 401 || status == 405 || status == 406
			if local.OK != want || local.StatusCode != status {
				t.Fatalf("本地 HTTP 分层异常：%+v", local)
			}
			metadata := checkCloudflareHTTP(server.URL, true)
			if metadata.OK != (status >= 200 && status < 300) {
				t.Fatalf("OAuth metadata 健康判断异常：%+v", metadata)
			}
		})
	}
	for _, scenario := range []struct {
		name    string
		public  cloudflareHealthItem
		oauth   cloudflareHealthItem
		recover bool
	}{
		{name: "公网502", public: cloudflareHealthItem{StatusCode: 502}, oauth: cloudflareHealthItem{OK: true}, recover: true},
		{name: "公网网络错误", oauth: cloudflareHealthItem{OK: true}, recover: true},
		{name: "公网403", public: cloudflareHealthItem{StatusCode: 403}, oauth: cloudflareHealthItem{OK: true}},
		{name: "元数据404", public: cloudflareHealthItem{OK: true}, oauth: cloudflareHealthItem{StatusCode: 404}},
		{name: "元数据502", public: cloudflareHealthItem{OK: true}, oauth: cloudflareHealthItem{StatusCode: 502}, recover: true},
		{name: "OAuth地址不一致", public: cloudflareHealthItem{OK: true}, oauth: cloudflareHealthItem{StatusCode: 200}},
	} {
		health := cloudflareHealth{LocalMCP: cloudflareHealthItem{OK: true}, TunnelProcess: cloudflareHealthItem{OK: true}, PublicMCP: scenario.public, OAuthMetadata: scenario.oauth}
		if cloudflareHealthNeedsAutoRecovery(health) != scenario.recover {
			t.Errorf("%s 恢复决策异常", scenario.name)
		}
	}
}

// TestCloudflareInvalidJSONObjects 验证非对象文件拒绝写回并保持原始字节。
func TestCloudflareInvalidJSONObjects(t *testing.T) {
	t.Setenv("MCPX_HOME", t.TempDir())
	_, path, _, _, _, _ := cloudflarePaths()
	for _, raw := range []string{"null", "[]", "true", "{} {}", ""} {
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := saveCloudflareDesktopConfig(defaultCloudflareDesktopConfig()); err == nil {
			t.Errorf("无效配置被覆盖：%q", raw)
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != raw {
			t.Fatalf("原始配置被改变：%q %v", raw, err)
		}
	}
}
