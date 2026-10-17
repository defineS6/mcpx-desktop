//go:build windows

package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/defineS6/mcpx-desktop/internal/config"
	"github.com/defineS6/mcpx-desktop/internal/fileutil"
	"github.com/defineS6/mcpx-desktop/internal/winproc"
)

var lifecycleMu sync.Mutex
var runtimeVersionPattern = regexp.MustCompile(`^mcpx\s+\S+`)
var runtimeVersionCache struct {
	sync.Mutex
	path    string
	mtime   time.Time
	size    int64
	version string
	err     error
}

type runtimeInfo struct {
	Path         string `json:"path"`
	ResolvedPath string `json:"resolved_path"`
	Available    bool   `json:"available"`
	Version      string `json:"version"`
	ConfigPath   string `json:"config_path"`
	Error        string `json:"error,omitempty"`
}

// stripUTF8BOM 兼容外部编辑器加入的 UTF-8 文件头；本程序写入时不生成 BOM。
func stripUTF8BOM(data []byte) []byte { return bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf}) }

// withLifecycle 在界面、托盘、自动恢复之间串行执行整项操作，内部函数不再重复加锁。
func withLifecycle(action func() error) error {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	return action()
}

// selfExecutable 只返回桌面管理器自身路径，供开机自启等管理功能使用。
func selfExecutable() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("定位桌面管理器失败：%w", err)
	}
	return filepath.Abs(path)
}

// managerConfigPath 将桌面专属设置与 Runtime 的 config.yaml 分开保存。
func managerConfigPath() (string, error) {
	home, err := config.HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "desktop-manager.json"), nil
}

// configuredRuntimePath 读取桌面设置；不创建文件，也不读取 Runtime 的任何凭据。
func configuredRuntimePath() (string, error) {
	path, err := managerConfigPath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var settings struct {
		RuntimePath string `json:"runtime_path"`
	}
	if err := json.Unmarshal(stripUTF8BOM(data), &settings); err != nil {
		return "", fmt.Errorf("解析桌面设置失败：%w", err)
	}
	return strings.TrimSpace(settings.RuntimePath), nil
}

// resolveRuntimePath 优先使用设置或 MCPX_RUNTIME，再发现同目录和 PATH 中的内核。
func resolveRuntimePath() (string, error) {
	configured, err := configuredRuntimePath()
	if err != nil {
		return "", err
	}
	manager, err := selfExecutable()
	if err != nil {
		return "", err
	}
	base := filepath.Dir(manager)
	if configured == "" {
		configured = strings.TrimSpace(os.Getenv("MCPX_RUNTIME"))
	}
	if configured != "" {
		if !filepath.IsAbs(configured) {
			configured = filepath.Join(base, configured)
		}
		return validateRuntimeFile(configured)
	}
	candidates := []string{filepath.Join(base, "mcpx.exe"), filepath.Join(base, "runtime", "mcpx.exe")}
	if found, err := exec.LookPath("mcpx.exe"); err == nil {
		candidates = append(candidates, found)
	}
	for _, candidate := range candidates {
		if path, err := validateRuntimeFile(candidate); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("未找到官方 mcpx.exe；请选择内核路径，或将它放在桌面管理器同目录")
}

// validateRuntimeFile 校验文件及完整路径，拒绝把管理器当成内核造成递归启动。
func validateRuntimeFile(path string) (string, error) {
	path = config.ExpandHome(strings.TrimSpace(path))
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("内核路径不可用：%w", err)
	}
	if !info.Mode().IsRegular() || !strings.EqualFold(filepath.Ext(absolute), ".exe") {
		return "", fmt.Errorf("请选择有效的 mcpx.exe 可执行文件")
	}
	if canonical, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = canonical
	}
	manager, err := selfExecutable()
	if err != nil {
		return "", err
	}
	managerInfo, err := os.Stat(manager)
	if err == nil && os.SameFile(info, managerInfo) {
		return "", fmt.Errorf("不能将桌面管理器自身设置为 MCPX 内核")
	}
	return absolute, nil
}

// inspectRuntimeVersion 限时调用版本命令并按文件身份缓存，防止轮询不断创建进程。
func inspectRuntimeVersion(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	runtimeVersionCache.Lock()
	defer runtimeVersionCache.Unlock()
	if runtimeVersionCache.path == path && runtimeVersionCache.mtime.Equal(info.ModTime()) && runtimeVersionCache.size == info.Size() {
		return runtimeVersionCache.version, runtimeVersionCache.err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, path, "-version")
	command.WaitDelay = time.Second
	winproc.ConfigureNoWindow(command)
	output, err := command.CombinedOutput()
	version := strings.TrimSpace(string(output))
	if index := strings.IndexByte(version, '\n'); index >= 0 {
		version = version[:index]
	}
	if err != nil {
		err = fmt.Errorf("读取 MCPX 版本失败：%w", err)
	} else if !runtimeVersionPattern.MatchString(version) {
		err = fmt.Errorf("所选程序未返回 MCPX 版本，请选择官方 mcpx.exe")
	}
	runtimeVersionCache.path, runtimeVersionCache.mtime, runtimeVersionCache.size = path, info.ModTime(), info.Size()
	runtimeVersionCache.version, runtimeVersionCache.err = version, err
	return version, err
}

// currentRuntimeInfo 汇总设置、发现和版本校验结果，供界面显示与启动前检查。
func currentRuntimeInfo() runtimeInfo {
	var result runtimeInfo
	var err error
	result.ConfigPath, err = managerConfigPath()
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Path, err = configuredRuntimePath()
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.ResolvedPath, err = resolveRuntimePath()
	if err == nil {
		result.Version, err = inspectRuntimeVersion(result.ResolvedPath)
	}
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Available = true
	return result
}

// saveRuntimePath 仅修改管理器的内核路径，保留未知设置字段，写入 UTF-8 无 BOM。
func saveRuntimePath(value string) error {
	value = strings.TrimSpace(value)
	if value != "" {
		var err error
		value, err = validateRuntimeFile(value)
		if err != nil {
			return err
		}
		if _, err = inspectRuntimeVersion(value); err != nil {
			return err
		}
	}
	path, err := managerConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fileutil.WithFileLock(path, func() error {
		settings := make(map[string]json.RawMessage)
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			if err := json.Unmarshal(stripUTF8BOM(data), &settings); err != nil {
				return fmt.Errorf("桌面设置已损坏，拒绝覆盖：%w", err)
			}
			if settings == nil {
				return fmt.Errorf("桌面设置必须是 JSON 对象")
			}
		}
		settings["runtime_path"], _ = json.Marshal(value)
		output, err := json.MarshalIndent(settings, "", "  ")
		if err != nil {
			return err
		}
		return fileutil.AtomicWrite(path, append(output, '\n'), 0o600)
	})
}
