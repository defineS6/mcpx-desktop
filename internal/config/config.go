// Package config 只管理桌面端使用的配置字段，其余 YAML 字段保持原样。
package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"

	"github.com/defineS6/mcpx-desktop/internal/fileutil"
	"gopkg.in/yaml.v3"
)

// ServerConfig 是桌面端管理的监听配置。
type ServerConfig struct {
	Host                       string `yaml:"host"`
	Port                       int    `yaml:"port"`
	DisableLocalhostProtection bool   `yaml:"disable_localhost_protection"`
	TrustProxyHeaders          bool   `yaml:"trust_proxy_headers"`
}

// OAuthConfig 是桌面端使用的 OAuth 地址配置。
type OAuthConfig struct {
	ServerURL string `yaml:"server_url"`
}

// AuthConfig 是桌面端管理的认证配置。
type AuthConfig struct {
	Mode  string      `yaml:"mode"`
	Token string      `yaml:"token"`
	OAuth OAuthConfig `yaml:"oauth"`
}

// WorkspaceEntry 表示一条工作区注册记录。
type WorkspaceEntry struct {
	Name        string `yaml:"name"`
	Path        string `yaml:"path"`
	Description string `yaml:"description"`
}

// LoggingConfig 是桌面端使用的日志目录配置。
type LoggingConfig struct {
	Dir string `yaml:"dir"`
}

// Config 保存受管字段及加载时的原文快照，私有信息不参与 YAML 编码。
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Auth       AuthConfig       `yaml:"auth"`
	Workspaces []WorkspaceEntry `yaml:"workspaces"`
	Logging    LoggingConfig    `yaml:"logging"`

	document *yaml.Node
	snapshot []byte
	original *managedSnapshot
}

type managedSnapshot struct {
	Server     ServerConfig
	Auth       AuthConfig
	Workspaces []WorkspaceEntry
	Logging    LoggingConfig
	Exists     bool
}

// HomeDir 返回 MCPX_HOME 指定的目录，未指定时使用用户主目录下的 .mcpx。
func HomeDir() (string, error) {
	if home := strings.TrimSpace(os.Getenv("MCPX_HOME")); home != "" {
		return filepath.Abs(ExpandHome(home))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("定位用户目录: %w", err)
	}
	return filepath.Join(home, ".mcpx"), nil
}

// GlobalConfigPath 返回全局 config.yaml 的路径。
func GlobalConfigPath() (string, error) {
	home, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "config.yaml"), nil
}

// ExpandHome 只展开以 ~ 开头的当前用户目录路径。
func ExpandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, `~\`) {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

// DefaultConfig 提供未创建配置文件时的监听与认证默认值。
func DefaultConfig() Config {
	return Config{Server: ServerConfig{Host: "127.0.0.1", Port: 9090}}
}

// EffectiveAuthMode 优先使用规范化的显式模式；未指定时仅根据 token 决定 bearer 或 open。
func EffectiveAuthMode(auth AuthConfig) string {
	mode := strings.ToLower(strings.TrimSpace(auth.Mode))
	if mode != "" {
		return mode
	}
	if auth.Token != "" {
		return "bearer"
	}
	return "open"
}

// ValidateAuthMode 只检查生效认证模式是否属于枚举，不限制凭据的配置时机。
func ValidateAuthMode(auth AuthConfig) error {
	switch EffectiveAuthMode(auth) {
	case "open", "bearer", "oauth", "dual":
		return nil
	default:
		return fmt.Errorf("无效鉴权模式 %q", auth.Mode)
	}
}

// LoadGlobal 读取全局 YAML 并保存原始文档和字节；缺失文件返回可安全写回的默认值。
func LoadGlobal(path string) (Config, error) {
	if path == "" {
		var err error
		path, err = GlobalConfigPath()
		if err != nil {
			return Config{}, err
		}
	}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	exists := err == nil
	cfg := DefaultConfig()
	cfg.original = &managedSnapshot{Exists: exists}
	if !exists {
		cfg.original.Server = cfg.Server
		cfg.original.Auth = cfg.Auth
		cfg.original.Logging = cfg.Logging
		return cfg, nil
	}
	cfg.snapshot = bytes.Clone(data)
	var doc yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})))
	if err := decoder.Decode(&doc); err != nil && err != io.EOF {
		return Config{}, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return Config{}, fmt.Errorf("配置 %s 不能包含多个 YAML 文档: %v", path, err)
	}
	if len(doc.Content) != 0 && doc.Content[0].Kind != yaml.MappingNode && doc.Content[0].Tag != "!!null" {
		return Config{}, fmt.Errorf("配置 %s 的根必须为映射", path)
	}
	if err := checkKeys(&doc, make(map[*yaml.Node]bool)); err != nil {
		return Config{}, fmt.Errorf("配置 %s: %w", path, err)
	}
	cfg.document = &doc
	root := rootMap(&doc)
	for _, keys := range [][]string{{}, {"server"}, {"auth"}, {"auth", "oauth"}, {"logging"}} {
		if err := rejectMerge(lookup(root, keys...)); err != nil {
			return Config{}, fmt.Errorf("受管配置不能使用 YAML 合并键: %w", err)
		}
	}
	for _, keys := range [][]string{{"server"}, {"auth"}, {"auth", "oauth"}, {"logging"}} {
		if node := lookup(root, keys...); node != nil && node.Kind != yaml.MappingNode && node.Tag != "!!null" {
			return Config{}, fmt.Errorf("配置字段 %s 必须是映射", strings.Join(keys, "."))
		}
	}
	if node := lookup(root, "workspaces"); node != nil && node.Tag != "!!null" {
		if node.Kind != yaml.SequenceNode {
			return Config{}, fmt.Errorf("配置字段 workspaces 必须是列表")
		}
		for i, entry := range node.Content {
			if entry.Kind != yaml.MappingNode {
				return Config{}, fmt.Errorf("workspaces 第 %d 条必须是映射", i+1)
			}
			if err := rejectMerge(entry); err != nil {
				return Config{}, fmt.Errorf("workspaces 第 %d 条: %w", i+1, err)
			}
		}
	}
	for _, field := range []struct {
		keys   []string
		target any
	}{
		{[]string{"server", "host"}, &cfg.Server.Host}, {[]string{"server", "port"}, &cfg.Server.Port},
		{[]string{"server", "disable_localhost_protection"}, &cfg.Server.DisableLocalhostProtection},
		{[]string{"server", "trust_proxy_headers"}, &cfg.Server.TrustProxyHeaders},
		{[]string{"auth", "mode"}, &cfg.Auth.Mode}, {[]string{"auth", "token"}, &cfg.Auth.Token},
		{[]string{"auth", "oauth", "server_url"}, &cfg.Auth.OAuth.ServerURL},
		{[]string{"logging", "dir"}, &cfg.Logging.Dir},
		{[]string{"workspaces"}, &cfg.Workspaces},
	} {
		node := lookup(root, field.keys...)
		if node != nil {
			if err := node.Decode(field.target); err != nil {
				return Config{}, fmt.Errorf("配置字段 %s: %w", strings.Join(field.keys, "."), err)
			}
		}
	}
	cfg.original.Server, cfg.original.Auth, cfg.original.Logging = cfg.Server, cfg.Auth, cfg.Logging
	if cfg.Workspaces != nil {
		cfg.original.Workspaces = make([]WorkspaceEntry, len(cfg.Workspaces))
		copy(cfg.original.Workspaces, cfg.Workspaces)
	}
	return cfg, nil
}

// WriteGlobal 对比加载时的受管字段，仅修改有变化的节点，并拒绝写入已被外部修改的文件。
func WriteGlobal(path string, cfg Config) error {
	if path == "" {
		var err error
		path, err = GlobalConfigPath()
		if err != nil {
			return err
		}
	}
	if cfg.original == nil {
		return fmt.Errorf("写入配置前必须先调用 LoadGlobal 获取文件快照")
	}
	return fileutil.WithFileLock(path, func() error {
		current, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("检查配置快照: %w", err)
		}
		exists := err == nil
		if exists != cfg.original.Exists || (exists && !bytes.Equal(current, cfg.snapshot)) {
			return fmt.Errorf("配置冲突：%s 已被其他进程修改，请重新加载后再保存", path)
		}
		var doc *yaml.Node
		if cfg.document == nil {
			doc = &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
		} else {
			doc = cloneNode(cfg.document)
		}
		root := rootMap(doc)
		old := cfg.original
		if !exists {
			// 首次创建配置时写出必需的默认监听字段。
			put(root, []string{"server", "host"}, cfg.Server.Host)
			put(root, []string{"server", "port"}, cfg.Server.Port)
		}
		for _, field := range []struct {
			keys          []string
			before, after any
		}{
			{[]string{"server", "host"}, old.Server.Host, cfg.Server.Host},
			{[]string{"server", "port"}, old.Server.Port, cfg.Server.Port},
			{[]string{"server", "disable_localhost_protection"}, old.Server.DisableLocalhostProtection, cfg.Server.DisableLocalhostProtection},
			{[]string{"server", "trust_proxy_headers"}, old.Server.TrustProxyHeaders, cfg.Server.TrustProxyHeaders},
			{[]string{"auth", "mode"}, old.Auth.Mode, cfg.Auth.Mode},
			{[]string{"auth", "token"}, old.Auth.Token, cfg.Auth.Token},
			{[]string{"auth", "oauth", "server_url"}, old.Auth.OAuth.ServerURL, cfg.Auth.OAuth.ServerURL},
			{[]string{"logging", "dir"}, old.Logging.Dir, cfg.Logging.Dir},
		} {
			if field.before != field.after {
				put(root, field.keys, field.after)
			}
		}
		if !reflect.DeepEqual(old.Workspaces, cfg.Workspaces) {
			if err := updateWorkspaces(root, cfg.Workspaces); err != nil {
				return err
			}
		}
		var output bytes.Buffer
		encoder := yaml.NewEncoder(&output)
		encoder.SetIndent(2)
		if err := encoder.Encode(doc); err != nil {
			return fmt.Errorf("生成配置: %w", err)
		}
		if err := encoder.Close(); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("创建配置目录: %w", err)
		}
		if exists {
			if err := fileutil.AtomicWrite(path+".bak", current, 0o600); err != nil {
				return fmt.Errorf("备份配置: %w", err)
			}
		}
		if err := fileutil.AtomicWrite(path, output.Bytes(), 0o600); err != nil {
			return fmt.Errorf("保存配置: %w", err)
		}
		return nil
	})
}

// RegisterWorkspace 按目录名称注册工作区，拒绝重名或重复路径，不修改工作区磁盘内容。
func RegisterWorkspace(path string, workspacePath string) error {
	if strings.TrimSpace(workspacePath) == "" {
		return fmt.Errorf("工作区路径不能为空")
	}
	absolute, err := filepath.Abs(ExpandHome(workspacePath))
	if err != nil {
		return fmt.Errorf("解析工作区路径: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return fmt.Errorf("检查工作区目录 %q: %w", absolute, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("工作区路径 %q 不是目录", absolute)
	}
	name := filepath.Base(absolute)
	cfg, err := LoadGlobal(path)
	if err != nil {
		return err
	}
	for _, entry := range cfg.Workspaces {
		if entry.Name == name || (runtime.GOOS == "windows" && strings.EqualFold(entry.Name, name)) {
			return fmt.Errorf("工作区名称 %q 已存在", name)
		}
		oldPath, err := filepath.Abs(ExpandHome(entry.Path))
		if err != nil {
			continue
		}
		if filepath.Clean(oldPath) == filepath.Clean(absolute) ||
			(runtime.GOOS == "windows" && strings.EqualFold(filepath.Clean(oldPath), filepath.Clean(absolute))) {
			return fmt.Errorf("工作区路径 %q 已注册", absolute)
		}
		if oldInfo, err := os.Stat(oldPath); err == nil && os.SameFile(info, oldInfo) {
			return fmt.Errorf("工作区路径 %q 已注册", absolute)
		}
	}
	cfg.Workspaces = append(cfg.Workspaces, WorkspaceEntry{Name: name, Path: absolute})
	return WriteGlobal(path, cfg)
}

// checkKeys 递归拒绝重复键和别名环，避免同名字段产生歧义。
func checkKeys(node *yaml.Node, active map[*yaml.Node]bool) error {
	if node == nil {
		return nil
	}
	if active[node] {
		return fmt.Errorf("YAML 别名存在循环")
	}
	active[node] = true
	defer delete(active, node)
	if node.Kind == yaml.AliasNode {
		return checkKeys(node.Alias, active)
	}
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]bool)
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if seen[key.Value] {
				return fmt.Errorf("YAML 存在重复键 %q", key.Value)
			}
			seen[key.Value] = true
		}
	}
	for _, child := range node.Content {
		if err := checkKeys(child, active); err != nil {
			return err
		}
	}
	return nil
}

// rejectMerge 禁止受管映射继承隐含字段，未知映射仍可保留 YAML 合并键。
func rejectMerge(node *yaml.Node) error {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(node.Content); i += 2 {
		if node.Content[i].Tag == "!!merge" {
			return fmt.Errorf("不支持 YAML 合并键")
		}
	}
	return nil
}

// rootMap 返回文档的顶层映射，空文档及 null 根节点以空映射表示。
func rootMap(doc *yaml.Node) *yaml.Node {
	if doc.Kind == 0 {
		doc.Kind = yaml.DocumentNode
	}
	if len(doc.Content) == 0 {
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	} else if doc.Content[0].Tag == "!!null" {
		root := doc.Content[0]
		root.Kind, root.Tag, root.Value, root.Style = yaml.MappingNode, "!!map", "", 0
		root.Content, root.Alias = nil, nil
	}
	return doc.Content[0]
}

// lookup 在映射树中查找字段，不改变原始节点。
func lookup(root *yaml.Node, keys ...string) *yaml.Node {
	for _, name := range keys {
		if root == nil || root.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for i := 0; i < len(root.Content); i += 2 {
			if root.Content[i].Value == name {
				next = root.Content[i+1]
				break
			}
		}
		root = next
	}
	return root
}

// put 仅更新目标叶节点，保留其余字段和注释。
func put(root *yaml.Node, keys []string, value any) {
	for _, name := range keys[:len(keys)-1] {
		next := lookup(root, name)
		if next == nil || next.Kind != yaml.MappingNode {
			next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setChild(root, name, next)
		}
		root = next
	}
	name := keys[len(keys)-1]
	target := lookup(root, name)
	if target == nil {
		target = &yaml.Node{}
		setChild(root, name, target)
	}
	target.Kind = yaml.ScalarNode
	target.Content = nil
	target.Alias = nil
	switch v := value.(type) {
	case string:
		target.Tag, target.Value = "!!str", v
	case int:
		target.Tag, target.Value, target.Style = "!!int", strconv.Itoa(v), 0
	case bool:
		target.Tag, target.Value, target.Style = "!!bool", strconv.FormatBool(v), 0
	}
}

// setChild 更新映射值，或将新键追加到映射尾部。
func setChild(root *yaml.Node, name string, value *yaml.Node) {
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == name {
			root.Content[i+1] = value
			return
		}
	}
	root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}, value)
}

// cloneNode 复制节点树，避免写入一次后修改原始快照；别名指向复制后的目标。
func cloneNode(node *yaml.Node) *yaml.Node {
	clones := make(map[*yaml.Node]*yaml.Node)
	var copyTree func(*yaml.Node) *yaml.Node
	copyTree = func(current *yaml.Node) *yaml.Node {
		if current == nil {
			return nil
		}
		if copied := clones[current]; copied != nil {
			return copied
		}
		clone := *current
		clones[current] = &clone
		clone.Content = make([]*yaml.Node, len(current.Content))
		for i, child := range current.Content {
			clone.Content[i] = copyTree(child)
		}
		clone.Alias = copyTree(current.Alias)
		return &clone
	}
	return copyTree(node)
}
