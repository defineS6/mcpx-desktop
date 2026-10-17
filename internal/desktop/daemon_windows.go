//go:build windows

package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/defineS6/mcpx-desktop/internal/config"
	"github.com/defineS6/mcpx-desktop/internal/winproc"
)

const (
	// 官方 CLI 写入的状态文件只包含 PID 与可执行路径，桌面不另建一套 Runtime 状态。
	daemonStateFilename = "mcpx-daemon.json"
	daemonLogFilename   = "mcpx-daemon.log"
	statusRunning       = "running"
	statusStarting      = "starting"
	statusConflict      = "conflict"
	statusStopped       = "stopped"
)

type daemonState struct {
	PID        int    `json:"pid"`
	Executable string `json:"executable"`
}

type serviceState struct {
	Status     string `json:"status"`
	PID        int    `json:"pid"`
	Addr       string `json:"addr"`
	Endpoint   string `json:"endpoint"`
	AuthMode   string `json:"auth_mode"`
	Executable string `json:"executable"`
	HomeDir    string `json:"home_dir"`
	LogPath    string `json:"log_path"`
	ConfigPath string `json:"config_path"`
	Error      string `json:"error,omitempty"`
}

// running 仅在受管进程存在且监听端口就绪时报告运行中。
func (s serviceState) running() bool { return s.Status == statusRunning }

// currentState 先核对 daemon PID 与完整镜像路径，再检测端口；不接管未受管进程。
// running 表示就绪，starting 表示端口未就绪，conflict 表示端口被未受管进程占用。
func currentState() serviceState {
	state := serviceState{Status: statusStopped}
	home, err := config.HomeDir()
	if err != nil {
		state.Error = err.Error()
		return state
	}
	state.HomeDir = home
	state.ConfigPath, _ = config.GlobalConfigPath()
	cfg, err := config.LoadGlobal("")
	if err != nil {
		cfg = config.DefaultConfig()
		state.Error = fmt.Sprintf("读取配置失败：%v", err)
	}
	logDir := strings.TrimSpace(cfg.Logging.Dir)
	if logDir == "" {
		logDir = filepath.Join(home, "logs")
	}
	state.LogPath = filepath.Join(config.ExpandHome(logDir), daemonLogFilename)
	host := strings.TrimSpace(cfg.Server.Host)
	if host == "" {
		host = "127.0.0.1"
	}
	port := cfg.Server.Port
	if port == 0 {
		port = 9090
	}
	state.Addr = net.JoinHostPort(host, strconv.Itoa(port))
	state.Endpoint = "http://" + probeAddress(state.Addr) + "/mcp"
	state.AuthMode = config.EffectiveAuthMode(cfg.Auth)
	persisted, err := readDaemonState(filepath.Join(home, daemonStateFilename))
	if err != nil {
		state.Status = untrackedStatus(state.Addr)
		return state
	}
	alive, matches := processAlive(persisted.PID, persisted.Executable)
	if !alive || !matches {
		state.Status = untrackedStatus(state.Addr)
		return state
	}
	state.PID, state.Executable = persisted.PID, persisted.Executable
	if portListening(state.Addr) {
		state.Status = statusRunning
	} else {
		state.Status = statusStarting
	}
	return state
}

// probeAddress 将全网卡监听地址映射到对应的本机回环地址，供 HTTP 与 TCP 检查使用。
func probeAddress(address string) string {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "::" {
		host = "::1"
	}
	return net.JoinHostPort(host, port)
}

// untrackedStatus 不会把端口占用者当作自己管理的 MCPX。
func untrackedStatus(addr string) string {
	if portListening(addr) {
		return statusConflict
	}
	return statusStopped
}

// readDaemonState 读取官方后台状态，拒绝缺少完整进程身份的记录。
func readDaemonState(path string) (daemonState, error) {
	var state daemonState
	data, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(stripUTF8BOM(data), &state); err != nil {
		return state, fmt.Errorf("解析后台状态失败：%w", err)
	}
	if state.PID <= 0 || !filepath.IsAbs(state.Executable) {
		return state, fmt.Errorf("后台状态缺少有效 PID 或完整程序路径")
	}
	return state, nil
}

// processAlive 通过 Win32 API 校验完整镜像路径；身份不明时绝不允许停止进程。
func processAlive(pid int, executable string) (alive bool, matches bool) {
	if !filepath.IsAbs(executable) {
		return false, false
	}
	alive, matches, err := winproc.State(pid, executable)
	if err != nil {
		return false, false
	}
	return alive, matches
}

// portListening 仅检查端口就绪，不将该检查用作进程归属证明。
func portListening(addr string) bool {
	connection, err := net.DialTimeout("tcp", probeAddress(addr), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}

// runRuntimeCommand 以无窗口方式调用外部内核，标准输入关闭且执行时间有上限。
func runRuntimeCommand(executable string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, args...)
	command.WaitDelay = time.Second
	command.Dir = filepath.Dir(executable)
	winproc.ConfigureNoWindow(command)
	output, err := command.CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		return text, fmt.Errorf("MCPX %s 失败：%w；%s", strings.Join(args, " "), err, text)
	}
	return text, nil
}

// startService 调用官方 -d；已运行时不重启，端口冲突或配置错误时拒绝启动。
func startService() error {
	state := currentState()
	if state.Error != "" {
		return fmt.Errorf("%s", state.Error)
	}
	if state.Status == statusRunning {
		return nil
	}
	if state.Status == statusStarting {
		return waitForStatus(statusRunning, 20*time.Second)
	}
	if state.Status == statusConflict {
		return fmt.Errorf("端口 %s 被未受管进程占用，拒绝启动", state.Addr)
	}
	runtime := currentRuntimeInfo()
	if !runtime.Available {
		return fmt.Errorf("%s", runtime.Error)
	}
	if _, err := runRuntimeCommand(runtime.ResolvedPath, "-d"); err != nil {
		return err
	}
	if err := waitForStatus(statusRunning, 20*time.Second); err != nil {
		// 启动失败后只清理路径匹配的本次受管进程，不留下后台残留。
		current := currentState()
		if managed(current) && strings.EqualFold(current.Executable, runtime.ResolvedPath) {
			_ = stopService()
		}
		return err
	}
	return nil
}

// stopService 优先使用该实例自身的官方 stop 命令；程序文件缺失时只终止已验证 PID。
func stopService() error {
	state := currentState()
	if !managed(state) {
		return nil
	}
	alive, matches := processAlive(state.PID, state.Executable)
	if !alive || !matches {
		return fmt.Errorf("进程身份已变化，拒绝停止 PID %d", state.PID)
	}
	if _, err := os.Stat(state.Executable); err == nil {
		if _, err := runRuntimeCommand(state.Executable, "stop"); err != nil {
			return err
		}
	} else {
		if _, err := runDesktopTaskkill(state.PID, true); err != nil {
			return err
		}
	}
	return waitForStatus(statusStopped, 10*time.Second)
}

// restartService 等旧实例完全停止后再启动外部内核，避免监听端口冲突。
func restartService() error {
	if err := stopService(); err != nil {
		return err
	}
	return startService()
}

// waitForStatus 把提前退出、配置失败和超时真实反馈给调用者，不再假报成功。
func waitForStatus(want string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		state := currentState()
		if state.Error != "" {
			return fmt.Errorf("%s", state.Error)
		}
		if state.Status == want {
			return nil
		}
		if want == statusRunning && (state.Status == statusStopped || state.Status == statusConflict) {
			return fmt.Errorf("MCPX 未启动成功（%s）；请查看 %s", state.Status, state.LogPath)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待 MCPX %s 超时；请查看 %s", want, state.LogPath)
		}
		time.Sleep(150 * time.Millisecond)
	}
}
