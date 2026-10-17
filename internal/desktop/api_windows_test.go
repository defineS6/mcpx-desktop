//go:build windows

package desktop

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/defineS6/mcpx-desktop/internal/config"
)

// desktopAPIRequest 通过真实路由分派请求并检查状态码与 JSON 响应。
func desktopAPIRequest(t *testing.T, a *api, method, path, body string, want int) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	a.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("%s %s：%d，期望 %d：%s", method, path, response.Code, want, response.Body.String())
	}
	if !json.Valid(response.Body.Bytes()) {
		t.Fatalf("响应不是 JSON：%s", response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("缺少禁止缓存响应头")
	}
	return response
}

// isolatedDesktopAPI 为每个 API 测试创建独立配置和必定不存在的内核路径。
func isolatedDesktopAPI(t *testing.T) *api {
	t.Helper()
	t.Setenv("MCPX_HOME", t.TempDir())
	t.Setenv("MCPX_RUNTIME", filepath.Join(t.TempDir(), "missing.exe"))
	return newAPI()
}

// TestAPIReadRoutes 验证冻结的只读路由与状态字段，不发现或启动本机服务。
func TestAPIReadRoutes(t *testing.T) {
	a := isolatedDesktopAPI(t)
	for path, keys := range map[string][]string{
		"/status":  {"status", "pid", "addr", "endpoint", "auth_mode", "executable", "home_dir", "log_path", "config_path"},
		"/config":  {"host", "port", "auth_mode", "token", "oauth_server_url", "effective_mode"},
		"/runtime": {"path", "resolved_path", "available", "version", "config_path", "error"},
	} {
		response := desktopAPIRequest(t, a, http.MethodGet, path, "", 200)
		var object map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &object); err != nil {
			t.Fatal(err)
		}
		for _, key := range keys {
			if _, ok := object[key]; !ok {
				t.Errorf("%s 缺字段 %s", path, key)
			}
		}
	}
	response := desktopAPIRequest(t, a, http.MethodGet, "/workspaces", "", 200)
	if strings.TrimSpace(response.Body.String()) != "[]" {
		t.Fatalf("初始工作区非空：%s", response.Body.String())
	}
}

// TestAPIConfigPreservesUnmanaged 验证连接配置保存只改受管字段并保留注释及未知字段。
func TestAPIConfigPreservesUnmanaged(t *testing.T) {
	a := isolatedDesktopAPI(t)
	path, err := config.GlobalConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	original := "# 用户配置注释\nserver:\n  host: 127.0.0.1\n  port: 19090 # 监听端口\n  disable_localhost_protection: true\n  trust_proxy_headers: true\n  future_server: keep-server\nauth:\n  mode: open\n  future_auth: keep-auth\n  oauth:\n    future_oauth: keep-oauth\nsecurity:\n  custom_policy: keep-policy\nlogging:\n  dir: isolated-logs\nretention:\n  days: 37\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	response := desktopAPIRequest(t, a, "PUT", "/config", `{"host":" 127.0.0.1 ","port":23456,"auth_mode":"bearer","token":" fakeBearerToken ","oauth_server_url":" https://example.invalid/// "}`, 200)
	var got connectionConfig
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Port != 23456 || got.Host != "127.0.0.1" || got.Token != "fakeBearerToken" || got.AuthMode != "bearer" || got.EffectiveMode != "bearer" || got.OAuthServerURL != "https://example.invalid" {
		t.Fatalf("配置响应不符：%+v", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, retained := range []string{"# 用户配置注释", "# 监听端口", "future_server: keep-server", "future_auth: keep-auth", "future_oauth: keep-oauth", "custom_policy: keep-policy", "days: 37", "disable_localhost_protection: true", "trust_proxy_headers: true", "dir: isolated-logs"} {
		if !bytes.Contains(data, []byte(retained)) {
			t.Errorf("保存丢失 %q：%s", retained, data)
		}
	}
}

// TestAPIWorkspaceLifecycle 验证注册、描述修改与删除只影响注册记录。
func TestAPIWorkspaceLifecycle(t *testing.T) {
	a := isolatedDesktopAPI(t)
	dir := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(marker, []byte("不得删除"), 0600); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"path": dir, "description": " 初始描述 "})
	response := desktopAPIRequest(t, a, "POST", "/workspaces", string(body), 200)
	var items []workspaceItem
	if err := json.Unmarshal(response.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "project" || items[0].Path != dir || items[0].Description != "初始描述" || items[0].Missing {
		t.Fatalf("注册结果：%+v", items)
	}
	response = desktopAPIRequest(t, a, "PATCH", "/workspaces/project", `{"description":" 更新描述 "}`, 200)
	if err := json.Unmarshal(response.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Description != "更新描述" {
		t.Fatalf("描述更新失败：%+v", items)
	}
	desktopAPIRequest(t, a, "DELETE", "/workspaces/project", "", 200)
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "不得删除" {
		t.Fatalf("删除注册影响磁盘：%q %v", data, err)
	}
	desktopAPIRequest(t, a, "DELETE", "/workspaces/project", "", 404)
}

// TestAPIRejectsInvalidRequests 验证非法输入、多个 JSON 值、跨站调用及未就绪对话框。
func TestAPIRejectsInvalidRequests(t *testing.T) {
	a := isolatedDesktopAPI(t)
	for _, body := range []string{`{"port":0}`, `{"port":-1}`, `{"port":65536}`, `{"port":"9090"}`, `{"port":9090,"auth_mode":"invalid"}`, `{`, `{"port":9090} {"port":9091}`} {
		desktopAPIRequest(t, a, "PUT", "/config", body, 400)
	}
	if path, err := config.GlobalConfigPath(); err != nil {
		t.Fatal(err)
	} else if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("非法请求写入配置：%v", err)
	}
	for _, path := range []string{"/runtime/pick", "/pick-directory"} {
		desktopAPIRequest(t, a, "POST", path, "", 503)
	}
	for _, method := range []string{"GET", "PUT"} {
		request := httptest.NewRequest(method, "/config", strings.NewReader(`{"port":9090}`))
		request.Header.Set("Sec-Fetch-Site", "cross-site")
		response := httptest.NewRecorder()
		a.ServeHTTP(response, request)
		if response.Code != 403 {
			t.Fatalf("跨站 %s 未拒绝：%d", method, response.Code)
		}
	}
}
