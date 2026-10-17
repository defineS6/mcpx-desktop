//go:build windows

package desktop

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/defineS6/mcpx-desktop/internal/config"
	"github.com/defineS6/mcpx-desktop/internal/fileutil"
	"github.com/defineS6/mcpx-desktop/internal/winproc"
	"golang.org/x/sys/windows"
)

const (
	cloudflareConfigFilename = "cloudflare-desktop.json"
	cloudflareStateFilename  = "cloudflare-desktop-state.json"
	cloudflareLogFilename    = "cloudflared.log"
	cloudflareBinaryFilename = "cloudflared.exe"

	cloudflareStatusRunning  = "running"
	cloudflareStatusStarting = "starting"
	cloudflareStatusStopped  = "stopped"
	cloudflareStatusError    = "error"

	cloudflareQuickReadyTimeout        = 30 * time.Second
	cloudflareHTTPTimeout              = 8 * time.Second
	cloudflareDownloadTimeout          = 3 * time.Minute
	cloudflareMaxDownloadBytes         = int64(200 << 20)
	cloudflareMaxMetadataBytes         = int64(4 << 20)
	cloudflareWatchdogCheckInterval    = 30 * time.Second
	cloudflareWatchdogFailureThreshold = 2
	cloudflareWatchdogRecoveryCooldown = 2 * time.Minute
)

var tryCloudflareURLPattern = regexp.MustCompile(`https://[a-zA-Z0-9-]+\.trycloudflare\.com`)
var cloudflaredSHA256Pattern = regexp.MustCompile(`(?i)\b[a-f0-9]{64}\b`)

var cloudflaredVersionMemo struct {
	sync.Mutex
	path    string
	modTime time.Time
	size    int64
	version string
}

// cloudflareDesktopConfig is intentionally stored outside config.yaml. Tunnel
// lifecycle is a Desktop concern; keeping it separate prevents desktop-only
// fields from becoming part of the MCP Runtime configuration contract.
type cloudflareDesktopConfig struct {
	Mode               string `json:"mode"`
	TunnelToken        string `json:"tunnel_token"`
	TunnelID           string `json:"tunnel_id"`
	PublicURL          string `json:"public_url"`
	LastPublicURL      string `json:"last_public_url"`
	ManageWithMCPX     bool   `json:"manage_with_mcpx"`
	SyncOAuthServerURL bool   `json:"sync_oauth_server_url"`
	AutoRecover        bool   `json:"auto_recover"`
	DesiredRunning     bool   `json:"desired_running"`
}

type cloudflareProcessState struct {
	PID        int    `json:"pid"`
	Executable string `json:"executable"`
	Mode       string `json:"mode"`
	TunnelID   string `json:"tunnel_id"`
	PublicURL  string `json:"public_url"`
}

type cloudflareSoftwareStatus struct {
	Installed bool   `json:"installed"`
	Managed   bool   `json:"managed"`
	Path      string `json:"path"`
	Version   string `json:"version"`
}

type cloudflareState struct {
	Status         string                   `json:"status"`
	PID            int                      `json:"pid"`
	Mode           string                   `json:"mode"`
	TunnelID       string                   `json:"tunnel_id"`
	PublicURL      string                   `json:"public_url"`
	PublicMCPURL   string                   `json:"public_mcp_url"`
	LocalMCPURL    string                   `json:"local_mcp_url"`
	OAuthServerURL string                   `json:"oauth_server_url"`
	OAuthLinked    bool                     `json:"oauth_linked"`
	LogPath        string                   `json:"log_path"`
	ConfigPath     string                   `json:"config_path"`
	Executable     string                   `json:"executable"`
	Software       cloudflareSoftwareStatus `json:"software"`
	Error          string                   `json:"error,omitempty"`
}

type cloudflareHealthItem struct {
	OK         bool   `json:"ok"`
	Detail     string `json:"detail"`
	URL        string `json:"url,omitempty"`
	StatusCode int    `json:"status_code,omitempty"`
}

type cloudflareHealth struct {
	OK            bool                 `json:"ok"`
	CheckedAt     string               `json:"checked_at"`
	Software      cloudflareHealthItem `json:"software"`
	LocalMCP      cloudflareHealthItem `json:"local_mcp"`
	TunnelProcess cloudflareHealthItem `json:"tunnel_process"`
	PublicMCP     cloudflareHealthItem `json:"public_mcp"`
	OAuthMetadata cloudflareHealthItem `json:"oauth_metadata"`
	OAuthLinked   bool                 `json:"oauth_linked"`
	Diagnosis     string               `json:"diagnosis,omitempty"`
}

func defaultCloudflareDesktopConfig() cloudflareDesktopConfig {
	return cloudflareDesktopConfig{
		Mode:               "quick",
		ManageWithMCPX:     false,
		SyncOAuthServerURL: true,
		AutoRecover:        true,
	}
}

func cloudflarePaths() (home, configPath, statePath, logPath, managedBinary string, err error) {
	home, err = config.HomeDir()
	if err != nil {
		return "", "", "", "", "", err
	}
	configPath = filepath.Join(home, cloudflareConfigFilename)
	statePath = filepath.Join(home, cloudflareStateFilename)
	logPath = filepath.Join(home, "logs", cloudflareLogFilename)
	managedBinary = filepath.Join(home, "bin", cloudflareBinaryFilename)
	return
}

func loadCloudflareDesktopConfig() (cloudflareDesktopConfig, error) {
	cfg := defaultCloudflareDesktopConfig()
	_, path, _, _, _, err := cloudflarePaths()
	if err != nil {
		return cfg, err
	}
	data, err := readCloudflareJSON(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("解析 %s 失败：%w", path, err)
	}
	if cfg.Mode == "" {
		cfg.Mode = "quick"
	}
	return cfg, nil
}

// setCloudflareDesiredRunning 在配置文件锁内完成读取、修改和写回，避免覆盖并发设置。
func setCloudflareDesiredRunning(desired bool) error {
	_, path, _, _, _, err := cloudflarePaths()
	if err != nil {
		return err
	}
	return fileutil.WithFileLock(path, func() error {
		cfg, err := loadCloudflareDesktopConfig()
		if err != nil || cfg.DesiredRunning == desired {
			return err
		}
		cfg.DesiredRunning = desired
		_, err = saveCloudflareDesktopConfigUnlocked(cfg)
		return err
	})
}

// saveCloudflareDesktopConfig 串行保存桌面配置，不覆盖尚未识别的 JSON 字段。
func saveCloudflareDesktopConfig(cfg cloudflareDesktopConfig) (cloudflareDesktopConfig, error) {
	_, path, _, _, _, err := cloudflarePaths()
	if err != nil {
		return cfg, err
	}
	var saved cloudflareDesktopConfig
	err = fileutil.WithFileLock(path, func() error {
		var writeErr error
		saved, writeErr = saveCloudflareDesktopConfigUnlocked(cfg)
		return writeErr
	})
	return saved, err
}

// saveCloudflareDesktopConfigUnlocked 仅在持有配置路径锁时调用，防止重复获取锁。
func saveCloudflareDesktopConfigUnlocked(cfg cloudflareDesktopConfig) (cloudflareDesktopConfig, error) {
	cfg.Mode = strings.ToLower(strings.TrimSpace(cfg.Mode))
	if cfg.Mode == "" {
		cfg.Mode = "quick"
	}
	if cfg.Mode != "quick" && cfg.Mode != "named" {
		return cfg, fmt.Errorf("Cloudflare 模式必须是 quick 或 named")
	}
	cfg.TunnelToken = strings.TrimSpace(cfg.TunnelToken)
	cfg.TunnelID = strings.TrimSpace(cfg.TunnelID)
	cfg.LastPublicURL = strings.TrimRight(strings.TrimSpace(cfg.LastPublicURL), "/")
	if strings.TrimSpace(cfg.PublicURL) != "" {
		origin, err := normalizePublicOrigin(cfg.PublicURL)
		if err != nil {
			return cfg, err
		}
		cfg.PublicURL = origin
	} else {
		cfg.PublicURL = ""
	}

	home, path, _, _, _, err := cloudflarePaths()
	if err != nil {
		return cfg, err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return cfg, err
	}
	if err := writeCloudflareJSON(path, cfg); err != nil {
		return cfg, fmt.Errorf("写入 Cloudflare Desktop 配置失败：%w", err)
	}
	return cfg, nil
}

func readCloudflareProcessState() (cloudflareProcessState, error) {
	var state cloudflareProcessState
	_, _, path, _, _, err := cloudflarePaths()
	if err != nil {
		return state, err
	}
	data, err := readCloudflareJSON(path)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("解析 Cloudflare 进程状态失败：%w", err)
	}
	return state, nil
}

// readCloudflareJSON 允许旧版 UTF-8 BOM，但拒绝非 JSON 对象和损坏文件。
func readCloudflareJSON(path string) ([]byte, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// 普通 os.ReadFile 的句柄不共享删除权限，会与并发原子替换冲突。
	// 允许替换后，本次读取仍指向完整的旧文件，后续读取则看到完整的新文件。
	handle, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(handle), path)
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("解析 %s 失败：JSON 必须是有效对象：%v", path, err)
	}
	return data, nil
}

// writeCloudflareJSON 保留原文件未知字段并以同目录原子替换写回；调用方必须持有路径锁。
func writeCloudflareJSON(path string, value any) error {
	fields := make(map[string]json.RawMessage)
	data, err := readCloudflareJSON(path)
	if err == nil {
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	known, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var updated map[string]json.RawMessage
	if err := json.Unmarshal(known, &updated); err != nil {
		return err
	}
	for key, raw := range updated {
		fields[key] = raw
	}
	output, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fileutil.AtomicWrite(path, output, 0o600)
}

// writeCloudflareProcessState 串行更新进程状态，保留其他版本写入的字段。
func writeCloudflareProcessState(state cloudflareProcessState) error {
	_, _, path, _, _, err := cloudflarePaths()
	if err != nil {
		return err
	}
	return fileutil.WithFileLock(path, func() error { return writeCloudflareJSON(path, state) })
}

// clearCloudflareProcessStateIfPID 在同一把锁内校验 PID 后清除，避免删除新进程的状态。
func clearCloudflareProcessStateIfPID(pid int) {
	_, _, path, _, _, err := cloudflarePaths()
	if err != nil {
		return
	}
	_ = fileutil.WithFileLock(path, func() error {
		data, err := readCloudflareJSON(path)
		if err != nil {
			return err
		}
		var state cloudflareProcessState
		if err := json.Unmarshal(data, &state); err != nil || state.PID != pid {
			return err
		}
		return os.Remove(path)
	})
}

// setCloudflareLastPublicURL 在配置锁内只更新 Quick Tunnel 的上次公网地址。
func setCloudflareLastPublicURL(origin string) error {
	_, path, _, _, _, err := cloudflarePaths()
	if err != nil {
		return err
	}
	return fileutil.WithFileLock(path, func() error {
		cfg, err := loadCloudflareDesktopConfig()
		if err != nil {
			return err
		}
		cfg.LastPublicURL = origin
		_, err = saveCloudflareDesktopConfigUnlocked(cfg)
		return err
	})
}

// setCloudflareProcessURLIfPID 只更新当前进程的 URL，不将已退出进程写回状态文件。
func setCloudflareProcessURLIfPID(pid int, origin string) error {
	_, _, path, _, _, err := cloudflarePaths()
	if err != nil {
		return err
	}
	return fileutil.WithFileLock(path, func() error {
		data, err := readCloudflareJSON(path)
		if err != nil {
			return err
		}
		var state cloudflareProcessState
		if err := json.Unmarshal(data, &state); err != nil {
			return err
		}
		if state.PID != pid {
			return fmt.Errorf("cloudflared 进程状态已改变")
		}
		state.PublicURL = origin
		return writeCloudflareJSON(path, state)
	})
}

func resolveCloudflared() (path string, managed bool) {
	_, _, _, _, managedPath, err := cloudflarePaths()
	if err == nil {
		if info, statErr := os.Stat(managedPath); statErr == nil && !info.IsDir() {
			return managedPath, true
		}
	}
	if found, lookErr := exec.LookPath("cloudflared"); lookErr == nil {
		return found, false
	}

	candidates := []string{
		filepath.Join(os.Getenv("ProgramFiles"), "cloudflared", cloudflareBinaryFilename),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "cloudflared", cloudflareBinaryFilename),
		filepath.Join(os.Getenv("ProgramFiles"), "Cloudflare", "Cloudflare Tunnel", cloudflareBinaryFilename),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Microsoft", "WinGet", "Links", cloudflareBinaryFilename),
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		candidates = append(candidates, filepath.Join(home, ".cloudflared", cloudflareBinaryFilename))
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate, false
		}
	}
	return "", false
}

func cloudflaredVersion(path string) string {
	if path == "" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	cloudflaredVersionMemo.Lock()
	if cloudflaredVersionMemo.path == path &&
		cloudflaredVersionMemo.modTime.Equal(info.ModTime()) &&
		cloudflaredVersionMemo.size == info.Size() {
		version := cloudflaredVersionMemo.version
		cloudflaredVersionMemo.Unlock()
		return version
	}
	cloudflaredVersionMemo.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, path, "--version")
	winproc.ConfigureNoWindow(command)
	output, err := command.CombinedOutput()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(output))
	if idx := strings.IndexByte(line, '\n'); idx >= 0 {
		line = strings.TrimSpace(line[:idx])
	}
	cloudflaredVersionMemo.Lock()
	cloudflaredVersionMemo.path = path
	cloudflaredVersionMemo.modTime = info.ModTime()
	cloudflaredVersionMemo.size = info.Size()
	cloudflaredVersionMemo.version = line
	cloudflaredVersionMemo.Unlock()
	return line
}

func currentCloudflareSoftwareStatus() cloudflareSoftwareStatus {
	path, managed := resolveCloudflared()
	return cloudflareSoftwareStatus{
		Installed: path != "",
		Managed:   managed,
		Path:      path,
		Version:   cloudflaredVersion(path),
	}
}

// cloudflaredAssetName 按当前 Windows 架构选取官方发布资源文件名。
func cloudflaredAssetName() (string, error) {
	var asset string
	switch runtime.GOARCH {
	case "amd64":
		asset = "cloudflared-windows-amd64.exe"
	case "arm64":
		asset = "cloudflared-windows-arm64.exe"
	default:
		return "", fmt.Errorf("当前 Windows 架构 %s 暂不支持 Desktop 自动安装 cloudflared", runtime.GOARCH)
	}
	return asset, nil
}

func installCloudflared() (cloudflareSoftwareStatus, error) {
	current := currentCloudflareState()
	if current.Status == cloudflareStatusRunning && current.Software.Managed {
		return current.Software, fmt.Errorf("请先停止使用 MCPX 受管 cloudflared 的 Tunnel，再更新 cloudflared")
	}
	asset, err := cloudflaredAssetName()
	if err != nil {
		return cloudflareSoftwareStatus{}, err
	}
	_, _, _, _, destination, err := cloudflarePaths()
	if err != nil {
		return cloudflareSoftwareStatus{}, err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return cloudflareSoftwareStatus{}, err
	}

	client := &http.Client{Timeout: cloudflareDownloadTimeout}
	if err := downloadVerifiedCloudflared(client, "https://api.github.com/repos/cloudflare/cloudflared/releases/latest", asset, destination); err != nil {
		return cloudflareSoftwareStatus{}, err
	}

	status := currentCloudflareSoftwareStatus()
	if !status.Installed || status.Path == "" {
		return status, fmt.Errorf("cloudflared 已下载，但安装验证失败")
	}
	return status, nil
}

// downloadVerifiedCloudflared 读取官方发布元数据，校验下载摘要后才原子替换旧文件。
func downloadVerifiedCloudflared(client *http.Client, metadataURL, assetName, destination string) error {
	req, err := http.NewRequest(http.MethodGet, metadataURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "MCPX Desktop cloudflared installer")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("读取 cloudflared 发布信息失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > cloudflareMaxMetadataBytes {
		return fmt.Errorf("cloudflared 发布信息异常：HTTP %s 或文件过大", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, cloudflareMaxMetadataBytes+1))
	if err != nil || int64(len(data)) > cloudflareMaxMetadataBytes {
		return fmt.Errorf("cloudflared 发布信息超过 4MB 或读取失败：%v", err)
	}
	var release struct {
		Body   string `json:"body"`
		Assets []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(data, &release); err != nil {
		return fmt.Errorf("解析 cloudflared 发布信息失败：%w", err)
	}
	for _, asset := range release.Assets {
		if asset.Name != assetName {
			continue
		}
		if asset.Size <= 0 || asset.Size > cloudflareMaxDownloadBytes {
			return fmt.Errorf("cloudflared 下载文件大小异常")
		}
		// 安装入口只接受 GitHub 官方发布的固定资源地址，防止元数据跳转到任意主机。
		tag := releaseTagFromURL(asset.URL, assetName)
		if tag == "" || asset.URL != "https://github.com/cloudflare/cloudflared/releases/download/"+tag+"/"+assetName {
			return fmt.Errorf("cloudflared 发布资源地址无效")
		}
		checksum := strings.TrimPrefix(strings.ToLower(asset.Digest), "sha256:")
		if !cloudflaredSHA256Pattern.MatchString(checksum) || len(checksum) != 64 || (asset.Digest != "" && !strings.HasPrefix(strings.ToLower(asset.Digest), "sha256:")) {
			checksum = sha256FromReleaseBody(release.Body, assetName)
		}
		if checksum == "" {
			return fmt.Errorf("cloudflared 发布信息缺少 %s 的可信 SHA256 摘要，拒绝安装", assetName)
		}
		return fetchVerifiedCloudflared(client, asset.URL, destination, checksum, asset.Size)
	}
	return fmt.Errorf("cloudflared 发布信息缺少 %s", assetName)
}

// releaseTagFromURL 只提取符合官方固定资源路径的单个版本段。
func releaseTagFromURL(raw, assetName string) string {
	prefix := "https://github.com/cloudflare/cloudflared/releases/download/"
	if !strings.HasPrefix(raw, prefix) || !strings.HasSuffix(raw, "/"+assetName) {
		return ""
	}
	tag := strings.TrimSuffix(strings.TrimPrefix(raw, prefix), "/"+assetName)
	if tag == "" || strings.ContainsAny(tag, "/?#%\\") {
		return ""
	}
	return tag
}

// sha256FromReleaseBody 仅接受正文中与资源文件名同一行对应的 SHA256。
func sha256FromReleaseBody(body, assetName string) string {
	for _, line := range strings.Split(body, "\n") {
		matchedAsset := false
		for _, field := range strings.Fields(line) {
			if strings.Trim(field, "*`():,[]") == assetName {
				matchedAsset = true
				break
			}
		}
		if !matchedAsset {
			continue
		}
		if hashes := cloudflaredSHA256Pattern.FindAllString(line, -1); len(hashes) == 1 {
			return strings.ToLower(hashes[0])
		}
	}
	return ""
}

// fetchVerifiedCloudflared 限量下载并校验 SHA256，失败时保留原有目标文件。
func fetchVerifiedCloudflared(client *http.Client, url, destination, checksum string, size int64) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "MCPX Desktop cloudflared installer")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("下载 cloudflared 失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > cloudflareMaxDownloadBytes {
		return fmt.Errorf("下载 cloudflared 失败：HTTP %s 或文件过大", resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(destination), "cloudflared-*.exe")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(resp.Body, cloudflareMaxDownloadBytes+1))
	if err != nil || written != size || written > cloudflareMaxDownloadBytes {
		return fmt.Errorf("cloudflared 下载不完整或文件过大：%v", err)
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), checksum) {
		return fmt.Errorf("cloudflared SHA256 校验失败，拒绝替换旧文件")
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	from, err := windows.UTF16PtrFromString(tmp.Name())
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return fmt.Errorf("替换 cloudflared 失败：%w", err)
	}
	return nil
}

func uninstallManagedCloudflared() (cloudflareSoftwareStatus, error) {
	state := currentCloudflareState()
	if state.Status == cloudflareStatusRunning || state.Status == cloudflareStatusStarting {
		return state.Software, fmt.Errorf("请先停止 Cloudflare Tunnel，再卸载 cloudflared")
	}
	_, _, _, _, managedPath, err := cloudflarePaths()
	if err != nil {
		return cloudflareSoftwareStatus{}, err
	}
	if err := os.Remove(managedPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return currentCloudflareSoftwareStatus(), fmt.Errorf("删除受管 cloudflared 失败：%w", err)
	}
	return currentCloudflareSoftwareStatus(), nil
}

func normalizePublicOrigin(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("公网地址必须是完整 HTTPS Origin，例如 https://mcp.example.com")
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return "", fmt.Errorf("Cloudflare 公网地址必须使用 https://")
	}
	if parsed.User != nil || parsed.ForceQuery || parsed.RawQuery != "" || strings.Contains(raw, "#") || (parsed.Path != "" && parsed.Path != "/") {
		return "", fmt.Errorf("公网地址只填写 Origin，不要追加 /mcp、查询参数或片段")
	}
	if parsed.Hostname() == "" || strings.ContainsAny(parsed.Hostname(), " \t\r\n") || strings.HasSuffix(parsed.Host, ":") || (strings.Contains(parsed.Host, ":") && parsed.Port() == "" && !strings.HasPrefix(parsed.Host, "[")) {
		return "", fmt.Errorf("公网地址的主机或端口无效")
	}
	if port := parsed.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return "", fmt.Errorf("公网地址的端口无效")
		}
	}
	return "https://" + parsed.Host, nil
}

func publicMCPURL(origin string) string {
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	if origin == "" {
		return ""
	}
	return origin + "/mcp"
}

func currentCloudflareState() cloudflareState {
	result := cloudflareState{
		Status:   cloudflareStatusStopped,
		Software: currentCloudflareSoftwareStatus(),
	}
	home, configPath, _, logPath, _, err := cloudflarePaths()
	if err != nil {
		result.Status = cloudflareStatusError
		result.Error = err.Error()
		return result
	}
	_ = home
	result.ConfigPath = configPath
	result.LogPath = logPath

	desktopCfg, cfgErr := loadCloudflareDesktopConfig()
	if cfgErr != nil {
		result.Error = cfgErr.Error()
	}
	result.Mode = desktopCfg.Mode
	result.TunnelID = desktopCfg.TunnelID
	if desktopCfg.Mode == "named" {
		result.PublicURL = desktopCfg.PublicURL
	}

	runtimeCfg, runtimeErr := config.LoadGlobal("")
	if runtimeErr == nil {
		host := strings.TrimSpace(runtimeCfg.Server.Host)
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		port := runtimeCfg.Server.Port
		if port == 0 {
			port = 9090
		}
		result.LocalMCPURL = fmt.Sprintf("http://%s/mcp", net.JoinHostPort(host, strconv.Itoa(port)))
		result.OAuthServerURL = strings.TrimRight(runtimeCfg.Auth.OAuth.ServerURL, "/")
	}

	processState, processErr := readCloudflareProcessState()
	if processErr == nil && processState.PID > 0 {
		alive, matches := processAlive(processState.PID, processState.Executable)
		if alive && matches {
			result.Status = cloudflareStatusRunning
			result.PID = processState.PID
			result.Executable = processState.Executable
			result.Mode = processState.Mode
			if processState.TunnelID != "" {
				result.TunnelID = processState.TunnelID
			}
			if processState.PublicURL != "" {
				result.PublicURL = processState.PublicURL
			}
		} else {
			clearCloudflareProcessStateIfPID(processState.PID)
		}
	}

	result.PublicMCPURL = publicMCPURL(result.PublicURL)
	result.OAuthLinked = result.PublicURL != "" && strings.EqualFold(result.OAuthServerURL, strings.TrimRight(result.PublicURL, "/"))
	return result
}

func ensureCloudflareRuntimeConfig(publicOrigin string, syncOAuth bool) (bool, error) {
	configPath, cfg, err := loadGlobalForWrite()
	if err != nil {
		return false, err
	}
	changed := false
	if !cfg.Server.DisableLocalhostProtection {
		cfg.Server.DisableLocalhostProtection = true
		changed = true
	}
	if !cfg.Server.TrustProxyHeaders {
		cfg.Server.TrustProxyHeaders = true
		changed = true
	}
	if syncOAuth && publicOrigin != "" {
		origin, err := normalizePublicOrigin(publicOrigin)
		if err != nil {
			return false, err
		}
		if strings.TrimRight(cfg.Auth.OAuth.ServerURL, "/") != origin {
			cfg.Auth.OAuth.ServerURL = origin
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	if err := config.WriteGlobal(configPath, cfg); err != nil {
		return false, fmt.Errorf("写入 Cloudflare 联动配置失败：%w", err)
	}
	return true, nil
}

func ensureMCPServiceForCloudflare() error {
	state := currentState()
	switch state.Status {
	case statusStopped:
		if err := startService(); err != nil {
			return fmt.Errorf("启动 MCPX 服务失败：%w", err)
		}
	case statusConflict:
		return fmt.Errorf("无法启动 Tunnel：MCPX 端口 %s 被未受管进程占用", state.Addr)
	}
	state = currentState()
	if state.Status != statusRunning {
		return fmt.Errorf("MCPX 服务尚未就绪（当前状态 %s）", state.Status)
	}
	return nil
}

// startMCPXStack is the single Desktop lifecycle entrypoint. Cloudflare can
// either follow the MCPX lifecycle or be managed manually from the Cloudflare
// page, depending on the Desktop tunnel configuration.
func startMCPXStack() error {
	desktopCfg, err := loadCloudflareDesktopConfig()
	if err != nil {
		return err
	}
	if !desktopCfg.ManageWithMCPX {
		return startService()
	}
	cloudflare := currentCloudflareState()
	if cloudflare.Status == cloudflareStatusRunning || cloudflare.Status == cloudflareStatusStarting {
		return ensureMCPServiceForCloudflare()
	}
	_, err = startCloudflareTunnel()
	return err
}

// stopMCPXStack follows the configured lifecycle policy. In linked mode it
// removes the public edge first; in manual mode it leaves the Tunnel alone.
func stopMCPXStack() error {
	desktopCfg, err := loadCloudflareDesktopConfig()
	if err != nil {
		return err
	}
	if !desktopCfg.ManageWithMCPX {
		return stopService()
	}

	var errs []error
	if err := stopCloudflareTunnel(); err != nil {
		errs = append(errs, fmt.Errorf("停止 Cloudflare Tunnel 失败：%w", err))
	}
	if err := stopService(); err != nil {
		errs = append(errs, fmt.Errorf("停止 MCPX 服务失败：%w", err))
	}
	return errors.Join(errs...)
}

func restartMCPXStack() error {
	desktopCfg, err := loadCloudflareDesktopConfig()
	if err != nil {
		return err
	}
	if !desktopCfg.ManageWithMCPX {
		return restartService()
	}
	if err := stopMCPXStack(); err != nil {
		return err
	}
	return startMCPXStack()
}

// validateCloudflareStartConfig 在启动服务和公网进程前检查模式、凭据及鉴权安全边界。
func validateCloudflareStartConfig(desktopCfg cloudflareDesktopConfig, runtimeCfg config.Config) error {
	if desktopCfg.Mode != "quick" && desktopCfg.Mode != "named" {
		return fmt.Errorf("Cloudflare 模式必须是 quick 或 named")
	}
	if err := config.ValidateAuthMode(runtimeCfg.Auth); err != nil {
		return err
	}
	if config.EffectiveAuthMode(runtimeCfg.Auth) == "open" {
		return fmt.Errorf("拒绝把 auth.mode=open 的 MCPX 暴露到公网；请先在“服务”页配置 bearer、oauth 或 dual")
	}
	if desktopCfg.Mode == "named" {
		if strings.TrimSpace(desktopCfg.TunnelToken) == "" {
			return fmt.Errorf("Named Tunnel 需要填写 Tunnel Token")
		}
		if strings.TrimSpace(desktopCfg.PublicURL) == "" {
			return fmt.Errorf("Named Tunnel 需要填写已经在 Cloudflare 配置好的公网 Origin")
		}
		if _, err := normalizePublicOrigin(desktopCfg.PublicURL); err != nil {
			return err
		}
	}
	return nil
}

func startCloudflareTunnel() (result cloudflareState, startErr error) {
	current := currentCloudflareState()
	if current.Status == cloudflareStatusRunning || current.Status == cloudflareStatusStarting {
		if err := setCloudflareDesiredRunning(true); err != nil {
			return current, err
		}
		return current, nil
	}

	desktopCfg, err := loadCloudflareDesktopConfig()
	if err != nil {
		return current, err
	}
	runtimeCfg, err := config.LoadGlobal("")
	if err != nil {
		return current, fmt.Errorf("读取 MCPX 配置失败：%w", err)
	}
	if err := validateCloudflareStartConfig(desktopCfg, runtimeCfg); err != nil {
		return current, err
	}
	if err := ensureMCPServiceForCloudflare(); err != nil {
		return current, err
	}

	// cloudflared brings requests in with the public Host. Enable the existing
	// reverse-proxy settings before exposing the port. For named tunnels the
	// public origin is already known, so OAuth can be pinned before launch.
	preOrigin := ""
	if desktopCfg.Mode == "named" {
		preOrigin = desktopCfg.PublicURL
	}
	changed, err := ensureCloudflareRuntimeConfig(preOrigin, desktopCfg.SyncOAuthServerURL)
	if err != nil {
		return current, err
	}
	if changed {
		if err := restartService(); err != nil {
			return current, fmt.Errorf("Cloudflare 联动配置已写入，但重启 MCPX 服务失败：%w", err)
		}
	}

	software := currentCloudflareSoftwareStatus()
	if !software.Installed {
		// Desktop 的“公网 MCP”入口应当是一键启动。首次使用时没有
		// cloudflared 就自动安装 MCPX 受管版本，避免用户必须先理解安装、
		// 本地服务、Tunnel 三个独立步骤。
		software, err = installCloudflared()
		if err != nil {
			return current, fmt.Errorf("自动安装 cloudflared 失败：%w", err)
		}
	}
	executable := software.Path
	if executable == "" {
		return current, fmt.Errorf("cloudflared 已检测/安装，但无法定位可执行文件")
	}
	localHost := strings.TrimSpace(runtimeCfg.Server.Host)
	if localHost == "" || localHost == "0.0.0.0" || localHost == "::" {
		localHost = "127.0.0.1"
	}
	localPort := runtimeCfg.Server.Port
	if localPort == 0 {
		localPort = 9090
	}
	localOrigin := "http://" + net.JoinHostPort(localHost, strconv.Itoa(localPort))
	var args []string
	token := ""
	if desktopCfg.Mode == "named" {
		args = []string{"tunnel", "--no-autoupdate", "--loglevel", "info", "--protocol", "http2", "run"}
		// Do not put the token in argv. cloudflared supports TUNNEL_TOKEN and this
		// keeps the credential out of process listings and Desktop logs.
		token = desktopCfg.TunnelToken
	} else {
		args = []string{"tunnel", "--protocol", "http2", "--url", localOrigin}
	}

	cmd, lines, exited, cancelTail, err := spawnCloudflared(executable, args, token, cloudflareProcessState{
		Mode:      desktopCfg.Mode,
		TunnelID:  desktopCfg.TunnelID,
		PublicURL: desktopCfg.PublicURL,
	})
	if err != nil {
		return currentCloudflareState(), err
	}

	defer cancelTail()
	// 本次启动出错时直接通过刚创建的进程句柄清理，避免状态文件变化后误杀其他 PID。
	started := false
	defer func() {
		if !started {
			if killErr := cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
				startErr = errors.Join(startErr, fmt.Errorf("清理 cloudflared 失败：%w", killErr))
			}
			select {
			case <-exited:
			case <-time.After(5 * time.Second):
				startErr = errors.Join(startErr, fmt.Errorf("等待 cloudflared 退出超时"))
			}
			result = currentCloudflareState()
		}
	}()
	publicOrigin := desktopCfg.PublicURL
	if desktopCfg.Mode == "quick" {
		publicOrigin, err = waitForTryCloudflareURL(cmd, lines, exited, cloudflareQuickReadyTimeout)
		if err != nil {
			return currentCloudflareState(), err
		}
		if err := setCloudflareLastPublicURL(publicOrigin); err != nil {
			return currentCloudflareState(), err
		}
		if err := setCloudflareProcessURLIfPID(cmd.Process.Pid, publicOrigin); err != nil {
			return currentCloudflareState(), err
		}

		changed, err = ensureCloudflareRuntimeConfig(publicOrigin, desktopCfg.SyncOAuthServerURL)
		if err != nil {
			return currentCloudflareState(), err
		}
		if changed {
			if err := restartService(); err != nil {
				return currentCloudflareState(), fmt.Errorf("已获得 Quick Tunnel 地址，但 OAuth 联动后重启 MCPX 失败：%w", err)
			}
		}
	} else {
		// Named 没有 URL 日志可等待，给立即失败的进程一个短暂观察窗口。
		select {
		case <-exited:
			return currentCloudflareState(), fmt.Errorf("cloudflared 启动后立即退出；请查看 Cloudflare 日志")
		case <-time.After(300 * time.Millisecond):
		}
	}
	alive, matches := processAlive(cmd.Process.Pid, cmd.Path)
	if !alive || !matches {
		return currentCloudflareState(), fmt.Errorf("cloudflared 启动后已退出；请查看 Cloudflare 日志")
	}
	if err := setCloudflareDesiredRunning(true); err != nil {
		return currentCloudflareState(), err
	}
	result = currentCloudflareState()
	if result.Status != cloudflareStatusRunning || result.PID != cmd.Process.Pid {
		return result, fmt.Errorf("cloudflared 启动后未保持运行；请查看 Cloudflare 日志")
	}
	started = true
	return result, nil
}

// spawnCloudflared 将日志文件句柄直接传给子进程，父进程退出也不会切断子进程的标准输出。
func spawnCloudflared(executable string, args []string, token string, processState cloudflareProcessState) (*exec.Cmd, <-chan string, <-chan struct{}, context.CancelFunc, error) {
	_, _, _, logPath, _, err := cloudflarePaths()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, nil, nil, nil, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	// Quick 从本次启动时的文件末尾开始读取；Named 不启动日志尾随协程。
	offset, err := logFile.Seek(0, io.SeekEnd)
	if err != nil {
		_ = logFile.Close()
		return nil, nil, nil, nil, err
	}
	// 状态文件记录实际启动的完整路径，与进程身份校验使用同一标准。
	resolved, err := exec.LookPath(executable)
	if err != nil {
		_ = logFile.Close()
		return nil, nil, nil, nil, err
	}
	executable, err = filepath.Abs(resolved)
	if err != nil {
		_ = logFile.Close()
		return nil, nil, nil, nil, err
	}
	command := exec.Command(executable, args...)
	winproc.ConfigureNoWindow(command)
	if token != "" {
		command.Env = append(os.Environ(), "TUNNEL_TOKEN="+token)
	}
	command.Stdout, command.Stderr = logFile, logFile
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		return nil, nil, nil, nil, fmt.Errorf("启动 cloudflared 失败：%w", err)
	}
	_ = logFile.Close()
	processState.PID = command.Process.Pid
	processState.Executable = executable
	if err := writeCloudflareProcessState(processState); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, nil, nil, nil, err
	}
	exited := make(chan struct{})
	go func(pid int) {
		err := command.Wait()
		if err != nil {
			appendCloudflareWatchdogLog("cloudflared PID %d 退出：%v", pid, err)
		}
		clearCloudflareProcessStateIfPID(pid)
		close(exited)
	}(command.Process.Pid)
	ctx, cancel := context.WithCancel(context.Background())
	if processState.Mode != "quick" {
		return command, nil, exited, cancel, nil
	}
	lines := make(chan string, 32)
	go tailCloudflaredLog(ctx, logPath, offset, exited, lines)
	return command, lines, exited, cancel, nil
}

// tailCloudflaredLog 只读取本次启动后新增的日志，保留跨次读取尚未换行的片段。
func tailCloudflaredLog(ctx context.Context, path string, offset int64, exited <-chan struct{}, lines chan<- string) {
	defer close(lines)
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	pending := ""
	readNew := func(flush bool) bool {
		buf := make([]byte, 32<<10)
		for {
			n, err := file.ReadAt(buf, offset)
			if n > 0 {
				offset += int64(n)
				pending += string(buf[:n])
				for end := strings.IndexByte(pending, '\n'); end >= 0; end = strings.IndexByte(pending, '\n') {
					select {
					case lines <- strings.TrimSpace(pending[:end]):
					case <-ctx.Done():
						return false
					}
					pending = pending[end+1:]
				}
				if len(pending) > 1<<20 {
					pending = pending[len(pending)-(1<<20):]
				}
			}
			if err != nil || n == 0 {
				break
			}
		}
		if flush && pending != "" {
			select {
			case lines <- pending:
			case <-ctx.Done():
				return false
			}
		}
		return true
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-exited:
			readNew(true)
			return
		case <-ticker.C:
			if !readNew(false) {
				return
			}
		}
	}
}

// waitForTryCloudflareURL 最多等待 30 秒；仅当进程仍存活时才接受日志中的 URL。
func waitForTryCloudflareURL(command *exec.Cmd, lines <-chan string, exited <-chan struct{}, timeout time.Duration) (string, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return "", fmt.Errorf("cloudflared 在返回 Quick Tunnel 公网地址前退出；请查看 Cloudflare 日志")
			}
			if match := tryCloudflareURLPattern.FindString(line); match != "" {
				select {
				case <-exited:
					return "", fmt.Errorf("cloudflared 返回公网地址后立即退出；请查看 Cloudflare 日志")
				default:
				}
				return strings.TrimRight(match, "/"), nil
			}
		case <-exited:
			return "", fmt.Errorf("cloudflared 在返回 Quick Tunnel 公网地址前退出；请查看 Cloudflare 日志")
		case <-timer.C:
			return "", fmt.Errorf("cloudflared 已启动，但 %s 内没有返回 trycloudflare.com 地址；请查看 Cloudflare 日志", timeout)
		}
	}
}

func stopCloudflareTunnel() error {
	return stopCloudflareTunnelWithDesired(false)
}

func stopCloudflareTunnelForRecovery() error {
	return stopCloudflareTunnelWithDesired(true)
}

func stopCloudflareTunnelWithDesired(desired bool) error {
	desiredErr := setCloudflareDesiredRunning(desired)
	processErr := stopCloudflareTunnelProcess()
	// 再写一次最终意图，关闭 watchdog 恰好在进程退出期间观察到旧进程并
	// 重新接管 DesiredRunning 的竞态窗口。
	finalDesiredErr := setCloudflareDesiredRunning(desired)
	return errors.Join(desiredErr, processErr, finalDesiredErr)
}

func stopCloudflareTunnelProcess() error {
	state, err := readCloudflareProcessState()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if state.PID <= 0 {
		clearCloudflareProcessStateIfPID(state.PID)
		return nil
	}
	alive, matches := processAlive(state.PID, state.Executable)
	if !alive {
		clearCloudflareProcessStateIfPID(state.PID)
		return nil
	}
	if !matches {
		// The state file is stale and Windows has reused the PID. Never kill the
		// replacement process; just discard our stale tracking record.
		clearCloudflareProcessStateIfPID(state.PID)
		return nil
	}

	gracefulOutput, gracefulErr := runDesktopTaskkill(state.PID, false)
	if waitDesktopProcessGone(state.PID, state.Executable, 750*time.Millisecond) {
		clearCloudflareProcessStateIfPID(state.PID)
		return nil
	}

	// taskkill /T can report success while a detached cloudflared process is
	// still alive. Escalate based on observed process state, not command exit.
	forceOutput, forceErr := runDesktopTaskkill(state.PID, true)
	if waitDesktopProcessGone(state.PID, state.Executable, 5*time.Second) {
		clearCloudflareProcessStateIfPID(state.PID)
		return nil
	}
	if forceErr != nil {
		return fmt.Errorf("停止 cloudflared PID %d 失败：强制停止 %v（%s）；普通停止 %v（%s）",
			state.PID, forceErr, forceOutput, gracefulErr, gracefulOutput)
	}
	return fmt.Errorf("cloudflared PID %d 在 taskkill /T /F 后仍然存活", state.PID)
}

func runDesktopTaskkill(pid int, force bool) (string, error) {
	args := []string{"/PID", strconv.Itoa(pid), "/T"}
	if force {
		args = append(args, "/F")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "taskkill", args...)
	winproc.ConfigureNoWindow(command)
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func waitDesktopProcessGone(pid int, executable string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		alive, matches := processAlive(pid, executable)
		if !alive || !matches {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// restartCloudflareAndMCP 本地 HTTP 正常时只恢复 Tunnel，否则重启整套服务。
func restartCloudflareAndMCP() error {
	local := checkLocalMCPHealth(currentState())
	if err := stopCloudflareTunnelForRecovery(); err != nil {
		return fmt.Errorf("停止 Cloudflare Tunnel 失败：%w", err)
	}
	if !local.OK {
		if err := restartService(); err != nil {
			return fmt.Errorf("重启 MCPX 服务失败：%w", err)
		}
	}
	if _, err := startCloudflareTunnel(); err != nil {
		return fmt.Errorf("重启 Cloudflare Tunnel 失败：%w", err)
	}
	return nil
}

func cloudflareHealthNeedsAutoRecovery(health cloudflareHealth) bool {
	if health.OK {
		return false
	}
	if !health.LocalMCP.OK {
		return true
	}
	// 配置错误的 4xx 不应通过自动重启来掩盖。
	for _, item := range []cloudflareHealthItem{health.PublicMCP, health.OAuthMetadata} {
		if !item.OK && item.StatusCode >= 400 && item.StatusCode < 500 {
			return false
		}
	}
	if !health.TunnelProcess.OK || !health.PublicMCP.OK {
		return true
	}
	return !health.OAuthMetadata.OK && (health.OAuthMetadata.StatusCode == 0 || health.OAuthMetadata.StatusCode >= 500)
}

func appendCloudflareWatchdogLog(format string, args ...any) {
	_, _, _, logPath, _, err := cloudflarePaths()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return
	}
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = fmt.Fprintf(file, "%s [watchdog] %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

func runCloudflareHealthCheck() cloudflareHealth {
	state := currentCloudflareState()
	runtimeCfg, _ := config.LoadGlobal("")
	result := cloudflareHealth{CheckedAt: time.Now().Format(time.RFC3339), OAuthLinked: state.OAuthLinked}

	result.Software = cloudflareHealthItem{OK: state.Software.Installed}
	if state.Software.Installed {
		result.Software.Detail = state.Software.Version
		if result.Software.Detail == "" {
			result.Software.Detail = state.Software.Path
		}
	} else {
		result.Software.Detail = "未检测到 cloudflared"
	}

	service := currentState()
	result.LocalMCP = checkLocalMCPHealth(service)
	result.TunnelProcess = cloudflareHealthItem{
		OK:     state.Status == cloudflareStatusRunning,
		Detail: fmt.Sprintf("cloudflared %s", state.Status),
	}
	if state.PID > 0 {
		result.TunnelProcess.Detail += fmt.Sprintf(" · pid=%d", state.PID)
	}

	if state.PublicMCPURL != "" {
		result.PublicMCP = checkCloudflareHTTP(state.PublicMCPURL, false)
	} else {
		result.PublicMCP = cloudflareHealthItem{Detail: "尚无公网 MCP URL"}
	}

	mode := config.EffectiveAuthMode(runtimeCfg.Auth)
	if (mode == "oauth" || mode == "dual") && state.PublicURL != "" {
		metadataURL := strings.TrimRight(state.PublicURL, "/") + "/.well-known/oauth-authorization-server"
		result.OAuthMetadata = checkCloudflareHTTP(metadataURL, true)
		if !state.OAuthLinked {
			result.OAuthMetadata.OK = false
			if result.OAuthMetadata.Detail != "" {
				result.OAuthMetadata.Detail += "；"
			}
			result.OAuthMetadata.Detail += "auth.oauth.server_url 与当前公网 Origin 不一致"
		}
	} else {
		result.OAuthMetadata = cloudflareHealthItem{OK: true, Detail: "当前鉴权模式无需 OAuth metadata 检查"}
	}

	result.OK = result.Software.OK && result.LocalMCP.OK && result.TunnelProcess.OK && result.PublicMCP.OK && result.OAuthMetadata.OK
	result.Diagnosis = diagnoseCloudflareHealth(result)
	return result
}

func checkLocalMCPHealth(service serviceState) cloudflareHealthItem {
	baseDetail := fmt.Sprintf("MCPX %s · %s", service.Status, service.Addr)
	if service.Status != statusRunning || strings.TrimSpace(service.Endpoint) == "" {
		return cloudflareHealthItem{Detail: baseDetail, URL: service.Endpoint}
	}

	item := checkCloudflareHTTP(service.Endpoint, false)
	if item.Detail != "" {
		item.Detail = baseDetail + " · HTTP " + item.Detail
	} else {
		item.Detail = baseDetail
	}
	return item
}

func diagnoseCloudflareHealth(result cloudflareHealth) string {
	switch {
	case !result.LocalMCP.OK:
		return "本地 MCPX HTTP 探测失败；优先检查 MCPX Runtime、监听端口和 mcpx-daemon.log。"
	case !result.Software.OK:
		return "未检测到 cloudflared；本地 MCPX 可达，但无法建立 Cloudflare Tunnel。"
	case !result.TunnelProcess.OK:
		return "本地 MCPX HTTP 正常，但 cloudflared Tunnel 未运行；优先检查或重启 Tunnel。"
	case !result.PublicMCP.OK && result.PublicMCP.StatusCode == http.StatusBadGateway:
		return "本地 MCPX HTTP 正常、Tunnel 进程存在，但公网 MCP 返回 502；优先检查 cloudflared.log 中的 origin 连接、EOF、connection reset 或 timeout。"
	case !result.PublicMCP.OK:
		return fmt.Sprintf("本地 MCPX HTTP 正常，但公网 MCP 异常（%s）；优先检查 Cloudflare Tunnel 与公网路由。", result.PublicMCP.Detail)
	case !result.OAuthMetadata.OK:
		return "MCP 与 Tunnel 可达，但 OAuth metadata 异常；检查 auth.oauth.server_url 与公网 Origin。"
	default:
		return "本地 MCPX、cloudflared Tunnel、公网 MCP 与 OAuth metadata 均正常。"
	}
}

func checkCloudflareHTTP(target string, requireOK bool) cloudflareHealthItem {
	item := cloudflareHealthItem{URL: target}
	client := &http.Client{
		Timeout: cloudflareHTTPTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		item.Detail = err.Error()
		return item
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "MCPX Desktop health-check")
	resp, err := client.Do(req)
	if err != nil {
		item.Detail = err.Error()
		return item
	}
	defer resp.Body.Close()
	item.StatusCode = resp.StatusCode
	item.Detail = resp.Status
	if requireOK {
		item.OK = resp.StatusCode >= 200 && resp.StatusCode < 300
		return item
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusAccepted, http.StatusNoContent,
		http.StatusBadRequest, http.StatusUnauthorized, http.StatusMethodNotAllowed, http.StatusNotAcceptable:
		item.OK = true
	}
	return item
}

func extractTryCloudflareURL(text string) string {
	return tryCloudflareURLPattern.FindString(text)
}
