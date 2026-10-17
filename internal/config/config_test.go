package config

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// testConfigPath 将每个用例的家目录和配置路径限制在临时目录内。
func testConfigPath(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return filepath.Join(home, "config.yaml")
}

// TestMissingAndDefaults 验证缺失配置不读取真实用户目录，首次写入生成可用配置。
func TestMissingAndDefaults(t *testing.T) {
	path := testConfigPath(t)
	cfg, err := LoadGlobal("")
	if err != nil || cfg.Server.Port != 9090 || cfg.Server.Host != "127.0.0.1" {
		t.Fatalf("默认配置：%+v, %v", cfg, err)
	}
	if err := WriteGlobal("", cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(data, []byte("port: 9090")) {
		t.Fatalf("首次写入：%q, %v", data, err)
	}
	if bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}) {
		t.Fatal("输出不应包含 BOM")
	}
	cfg, err = LoadGlobal(path)
	if err != nil || cfg.Server.Port != 9090 {
		t.Fatalf("重新加载：%+v, %v", cfg, err)
	}
}

// TestPreserveUnknownAndComments 验证受管字段增量写入保留未知字段、注释及工作区扩展字段。
func TestPreserveUnknownAndComments(t *testing.T) {
	path := testConfigPath(t)
	original := "# 顶部注释\nserver:\n  host: localhost # 地址注释\n  port: 9000\n  extra: keep\nauth:\n  mode: open\n  oauth:\n    server_url: ''\n    extra: authkeep\nworkspaces:\n  - name: one # 工作区注释\n    path: /tmp/one\n    description: old\n    extra: entrykeep\n  - name: two\n    path: /tmp/two\n    extra: secondkeep\nlogging:\n  dir: custom\n  level: debug\nunknown:\n  enabled: true\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Server.Port = 9010
	cfg.Workspaces[0].Description = "new"
	cfg.Workspaces = append([]WorkspaceEntry{cfg.Workspaces[1], cfg.Workspaces[0]}, WorkspaceEntry{Name: "three", Path: "/tmp/three"})
	if err := WriteGlobal(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# 顶部注释", "# 地址注释", "# 工作区注释", "extra: keep", "extra: authkeep", "extra: entrykeep", "extra: secondkeep", "level: debug", "enabled: true", "description: new", "port: 9010"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("丢失 %q：\n%s", want, data)
		}
	}
	backup, err := os.ReadFile(path + ".bak")
	if err != nil || string(backup) != original {
		t.Fatalf("备份：%q, %v", backup, err)
	}
	var parsed struct {
		Workspaces []WorkspaceEntry `yaml:"workspaces"`
	}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Workspaces) != 3 || parsed.Workspaces[0].Name != "two" {
		t.Fatalf("工作区顺序：%+v", parsed.Workspaces)
	}
}

// TestConflictAndBOM 验证快照冲突不覆盖文件，BOM 输入可读并无 BOM 输出。
func TestConflictAndBOM(t *testing.T) {
	path := testConfigPath(t)
	bom := append([]byte{0xef, 0xbb, 0xbf}, []byte("# 保留\nserver:\n  port: 8001\n")...)
	if err := os.WriteFile(path, bom, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGlobal(path)
	if err != nil || cfg.Server.Port != 8001 {
		t.Fatalf("读取 BOM：%+v, %v", cfg, err)
	}
	if err := os.WriteFile(path, []byte("server:\n  port: 9999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Server.Port = 8002
	if err := WriteGlobal(path, cfg); err == nil || !strings.Contains(err.Error(), "冲突") {
		t.Fatalf("应拒绝冲突：%v", err)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("冲突不应生成备份：%v", err)
	}
	data, _ := os.ReadFile(path)
	if !bytes.Contains(data, []byte("9999")) {
		t.Fatalf("原文件被改写：%s", data)
	}
	if err := os.WriteFile(path, bom, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteGlobal(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}) || !bytes.Contains(data, []byte("# 保留")) {
		t.Fatalf("BOM 输出：%q", data)
	}
	backup, _ := os.ReadFile(path + ".bak")
	if !bytes.Equal(backup, bom) {
		t.Fatalf("备份应是原始字节：%q", backup)
	}
}

// TestInvalidYAML 验证重复键和多文档都被拒绝。
func TestInvalidYAML(t *testing.T) {
	path := testConfigPath(t)
	for _, text := range []string{"unknown:\n  key: a\n  key: b\n", "server: {}\n---\nserver: {}\n", "auth:\n  oauth: {}\n  oauth: {}\n", "server: wrong-type\n"} {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadGlobal(path); err == nil {
			t.Errorf("未拒绝非法配置：%s", text)
		}
	}
}

// TestRegisterWorkspace 验证按名称与路径去重，且不删除或修改工作区目录。
func TestRegisterWorkspace(t *testing.T) {
	path := testConfigPath(t)
	one := filepath.Join(t.TempDir(), "project")
	two := filepath.Join(t.TempDir(), "project")
	for _, dir := range []string{one, two} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := RegisterWorkspace("", one); err != nil {
		t.Fatal(err)
	}
	if err := RegisterWorkspace(path, one); err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("重复：%v", err)
	}
	if err := RegisterWorkspace(path, two); err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("同名：%v", err)
	}
	if _, err := os.Stat(one); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGlobal(path)
	if err != nil || len(cfg.Workspaces) != 1 {
		t.Fatalf("注册结果：%+v, %v", cfg.Workspaces, err)
	}
}

// TestRegisterWorkspacePaths 验证不存在或非目录的路径被拒绝，实际同目录路径不会重复注册。
func TestRegisterWorkspacePaths(t *testing.T) {
	path := testConfigPath(t)
	base := t.TempDir()
	regular := filepath.Join(base, "regular")
	if err := os.WriteFile(regular, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, path string }{
		{name: "不存在", path: filepath.Join(base, "missing")},
		{name: "普通文件", path: regular},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := RegisterWorkspace(path, tc.path); err == nil {
				t.Fatal("应拒绝非现存目录")
			}
		})
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("失败注册不应创建配置：%v", err)
	}
	if runtime.GOOS != "windows" {
		return
	}
	project := filepath.Join(base, "project")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workspaces = []WorkspaceEntry{{Name: "another", Path: project}}
	if err := WriteGlobal(path, cfg); err != nil {
		t.Fatal(err)
	}
	if err := RegisterWorkspace(path, strings.ToUpper(project)); err == nil || !strings.Contains(err.Error(), "已注册") {
		t.Fatalf("Windows 大小写变体应按同一路径去重：%v", err)
	}
}

// TestMissingConflict 验证加载时缺失、写入前被其他进程创建的文件不会被覆盖。
func TestMissingConflict(t *testing.T) {
	path := testConfigPath(t)
	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new: external\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteGlobal(path, cfg); err == nil {
		t.Fatal("应拒绝外部新建文件")
	}
}

// TestWorkspaceIdentity 验证优先匹配原有记录，改名、删除时不串用工作区扩展字段。
func TestWorkspaceIdentity(t *testing.T) {
	path := testConfigPath(t)
	original := "workspaces:\n  - name: a\n    path: /a\n    extra: from-a\n  - name: b\n    path: /b\n    extra: from-b\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workspaces = []WorkspaceEntry{{Name: "b", Path: "/a"}, {Name: "b", Path: "/b"}}
	if err := WriteGlobal(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "extra: from-a") || !strings.Contains(string(data), "extra: from-b") {
		t.Fatalf("原节点串用：%s", data)
	}
	cfg, err = LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workspaces = cfg.Workspaces[1:]
	if err := WriteGlobal(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil || strings.Contains(string(data), "from-a") || !strings.Contains(string(data), "from-b") {
		t.Fatalf("删除工作区记录：%s, %v", data, err)
	}
}

// TestNullWorkspace 验证已有的空值配置可继续新增工作区。
func TestNullWorkspace(t *testing.T) {
	path := testConfigPath(t)
	if err := os.WriteFile(path, []byte("server: null\nworkspaces: null\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workspaces = []WorkspaceEntry{{Name: "one", Path: "/one"}}
	if err := WriteGlobal(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(data, []byte("name: one")) {
		t.Fatalf("空工作区写入：%s, %v", data, err)
	}
}

// TestEmptyDocuments 验证首次写入空文档和 null 根节点时不会崩溃。
func TestEmptyDocuments(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{name: "空文件"},
		{name: "空白文档", content: "  \n"},
		{name: "null 根节点", content: "null\n"},
		{name: "波浪线根节点", content: "~\n"},
		{name: "null 受管字段", content: "server: null\nworkspaces: null\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := testConfigPath(t)
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadGlobal(path)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Server.Port = 9091
			if err := WriteGlobal(path, cfg); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadGlobal(path)
			if err != nil || loaded.Server.Port != 9091 {
				t.Fatalf("写回空文档：%+v, %v", loaded, err)
			}
		})
	}
}

// TestUnknownAlias 验证修改受管字段及工作区记录时保留未知锚点和别名。
func TestUnknownAlias(t *testing.T) {
	path := testConfigPath(t)
	text := "details: &shared\n  label: keep\nunknown: *shared\nworkspaces:\n  - name: one\n    path: /one\n    extra: *shared\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workspaces[0].Description = "updated"
	cfg.Server.Port = 9001
	if err := WriteGlobal(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"details: &shared", "label: keep", "unknown: *shared", "extra: *shared", "description: updated"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("未知字段或别名丢失 %q：%s", want, data)
		}
	}
	if _, err := LoadGlobal(path); err != nil {
		t.Fatalf("写回后别名不可用：%v", err)
	}
}

// TestUnknownMerge 验证未知字段内的合并键可保留，受管映射的合并键会被拒绝。
func TestUnknownMerge(t *testing.T) {
	path := testConfigPath(t)
	text := "base: &base\n  color: blue\nunknown:\n  <<: *base\nserver:\n  port: 9090\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Server.Port = 9091
	if err := WriteGlobal(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(data, []byte("<<: *base")) {
		t.Fatalf("未知字段合并键丢失：%s, %v", data, err)
	}
	if err := os.WriteFile(path, []byte("base: &base\n  port: 9090\nserver:\n  <<: *base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGlobal(path); err == nil {
		t.Fatal("受管字段的合并键应被拒绝")
	}
}

// TestAuthModes 验证模式规范化、默认值及只校验枚举的官方语义。
func TestAuthModes(t *testing.T) {
	cases := []struct {
		name    string
		auth    AuthConfig
		mode    string
		invalid bool
	}{
		{name: "默认开放", mode: "open"},
		{name: "仅服务地址仍开放", auth: AuthConfig{OAuth: OAuthConfig{ServerURL: "https://example.org"}}, mode: "open"},
		{name: "仅令牌", auth: AuthConfig{Token: "secret"}, mode: "bearer"},
		{name: "令牌与服务地址仍为 bearer", auth: AuthConfig{Token: "secret", OAuth: OAuthConfig{ServerURL: "https://example.org"}}, mode: "bearer"},
		{name: "空白模式按默认令牌推导", auth: AuthConfig{Mode: "  ", Token: "secret"}, mode: "bearer"},
		{name: "显式开放覆盖令牌", auth: AuthConfig{Mode: " OPEN ", Token: "secret"}, mode: "open"},
		{name: "显式 bearer 无需令牌", auth: AuthConfig{Mode: " BeArEr "}, mode: "bearer"},
		{name: "显式 OAuth 无需服务地址", auth: AuthConfig{Mode: " OAUTH "}, mode: "oauth"},
		{name: "显式双模式无需凭据", auth: AuthConfig{Mode: " DuAl "}, mode: "dual"},
		{name: "非法模式", auth: AuthConfig{Mode: " Other ", Token: "secret"}, mode: "other", invalid: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if mode := EffectiveAuthMode(tc.auth); mode != tc.mode {
				t.Fatalf("实际模式 %q，期望 %q", mode, tc.mode)
			}
			if err := ValidateAuthMode(tc.auth); (err != nil) != tc.invalid {
				t.Fatalf("模式校验错误：%v", err)
			}
		})
	}
}

// TestAuthAndSelectiveEdit 验证字段单独编辑不会补写其它受管字段。
func TestAuthAndSelectiveEdit(t *testing.T) {
	path := testConfigPath(t)
	if err := os.WriteFile(path, []byte("# 原注释\nserver:\n  port: 9000\ncustom: keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Server.Port = 9001
	if err := WriteGlobal(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(data), "host:") || strings.Contains(string(data), "auth:") || !strings.Contains(string(data), "custom: keep") {
		t.Fatalf("出现额外配置字段：%s, %v", data, err)
	}
}
