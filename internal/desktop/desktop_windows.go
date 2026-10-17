//go:build windows

// Package desktop 提供独立管理器的系统托盘与图形界面。
//
// 设计要点：托盘通过调用外部 mcpx.exe 的 -d 和 stop 管理后台服务。
// 退出管理器不停止 Runtime 或 Tunnel，桌面端不内嵌 MCP 服务实现。
package desktop

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/defineS6/mcpx-desktop/internal/config"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"golang.org/x/sys/windows/registry"
)

//go:embed all:frontend/dist
var frontendAssets embed.FS

//go:embed assets/tray-running.png
var iconRunning []byte

//go:embed assets/tray-starting.png
var iconStarting []byte

//go:embed assets/tray-stopped.png
var iconStopped []byte

// statusPollInterval 是托盘刷新服务状态的间隔。2s 足够跟手，又不至于让
// tasklist 调用变成持续的 CPU 负担。
const statusPollInterval = 2 * time.Second

type desktopApp struct {
	app    *application.App
	tray   *application.SystemTray
	window *application.WebviewWindow
	api    *api
	done   chan struct{}

	mu                   sync.Mutex
	last                 serviceState
	lastCloudflare       cloudflareState
	lastCloudflareHealth *cloudflareHealth
}

// trayOnlyFlag 让托盘静默驻留、不弹主窗口；开机自启只注册独立管理器 -tray。
const trayOnlyFlag = "tray"

// Run 启动托盘与 GUI，直到用户从托盘退出为止。
func Run(args []string) error {
	handler, err := frontendHandler(args)
	if err != nil {
		return err
	}
	home, err := config.HomeDir()
	if err != nil {
		return err
	}
	windowsOptions := application.WindowsOptions{WebviewUserDataPath: filepath.Join(home, "desktop-webview")}
	for _, arg := range args {
		if strings.HasPrefix(arg, "-inspect-port=") {
			port, err := strconv.Atoi(strings.TrimPrefix(arg, "-inspect-port="))
			if err != nil || port < 1 || port > 65535 {
				return fmt.Errorf("开发调试端口必须在 1-65535 之间")
			}
			windowsOptions.AdditionalBrowserArgs = []string{fmt.Sprintf("--remote-debugging-port=%d", port), "--remote-debugging-address=127.0.0.1"}
		}
	}
	identity := sha256.Sum256([]byte(strings.ToLower(home)))
	trayOnly := hasFlag(args, trayOnlyFlag)
	desk := &desktopApp{api: newAPI(), done: make(chan struct{})}
	defer close(desk.done)

	desk.app = application.New(application.Options{
		Name:        "MCPX Desktop",
		Description: "独立的 MCPX Runtime 与 Cloudflare Tunnel 管理器",
		Windows:     windowsOptions,
		Icon:        iconRunning,
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: fmt.Sprintf("io.defines6.mcpx-desktop.%x", identity[:12]),
			OnSecondInstanceLaunch: func(application.SecondInstanceData) {
				desk.showWindow()
			},
		},
		Assets: application.AssetOptions{
			Handler: handler,
		},
		Services: []application.Service{
			application.NewServiceWithOptions(desk.api, application.ServiceOptions{Route: "/api"}),
		},
	})
	desk.api.setApp(desk.app)

	desk.setupWindow(trayOnly)
	desk.setupTray()
	go desk.pollStatus()
	go desk.pollCloudflareHealth()

	return desk.app.Run()
}

// hasFlag 接受 -name 和 --name 两种写法，和标准库 flag 的习惯一致。
func hasFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == "-"+name || arg == "--"+name {
			return true
		}
	}
	return false
}

func (d *desktopApp) setupWindow(trayOnly bool) {
	d.window = d.app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             "main",
		Title:            "MCPX Desktop",
		Width:            940,
		Height:           660,
		MinWidth:         720,
		MinHeight:        480,
		Hidden:           trayOnly,
		BackgroundColour: initialBackground(),
	})

	// 关闭按钮只隐藏窗口。托盘还在，服务也还在——直接销毁窗口会让用户以为
	// 整个程序退出了。真正的退出走托盘菜单。
	d.window.RegisterHook(events.Common.WindowClosing, func(event *application.WindowEvent) {
		event.Cancel()
		d.window.Hide()
	})
}

func (d *desktopApp) setupTray() {
	d.tray = d.app.SystemTray.New()
	d.tray.SetIcon(iconStopped)
	d.tray.SetTooltip("MCPX")
	d.tray.OnClick(func() { d.showWindow() })
	d.refresh(currentState(), true)
}

func (d *desktopApp) showWindow() {
	d.window.Show()
	d.window.Focus()
}

// initialBackground 返回窗口底色。WebView 加载完成前这层底色是可见的，
// 跟系统主题对齐才不会在浅色环境下闪一下深色。
//
// 这里只看系统偏好：界面里手动切换的主题存在 WebView 的 localStorage 里，
// Go 侧读不到。手动覆盖与系统相反时，首帧仍可能有一次极短的色差。
func initialBackground() application.RGBA {
	if systemPrefersDark() {
		return application.NewRGBA(16, 18, 22, 255) // 对应 CSS 的 --bg 深色值
	}
	return application.NewRGBA(244, 246, 248, 255) // 对应 --bg 浅色值
}

// systemPrefersDark 读取 Windows 的"应用模式"偏好。读不到时按深色处理，
// 与前端 CSS 的默认值保持一致。
func systemPrefersDark() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return true
	}
	defer key.Close()
	// AppsUseLightTheme: 0=深色应用, 1=浅色应用
	value, _, err := key.GetIntegerValue("AppsUseLightTheme")
	if err != nil {
		return true
	}
	return value == 0
}

// pollStatus 周期性刷新服务状态，仅在状态真正变化时才重建托盘菜单。
func (d *desktopApp) pollStatus() {
	ticker := time.NewTicker(statusPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.done:
			return
		case <-ticker.C:
			d.refresh(currentState(), false)
		}
	}
}

// pollCloudflareHealth 在 Desktop 驻留期间定时探测公网链路。连续失败才恢复，
// 避免 Cloudflare 短暂抖动触发重启；手动停止会把 DesiredRunning 清掉，因此
// watchdog 不会违背用户的停止意图。
func (d *desktopApp) pollCloudflareHealth() {
	ticker := time.NewTicker(cloudflareWatchdogCheckInterval)
	defer ticker.Stop()

	failures := 0
	var lastRecovery time.Time
	for {
		select {
		case <-d.done:
			return
		case <-ticker.C:
		}
		cfg, err := loadCloudflareDesktopConfig()
		if err != nil || !cfg.AutoRecover {
			failures = 0
			continue
		}

		// 不从观察到的进程反推运行意图，避免手动停止与自动恢复竞争。
		if !cfg.DesiredRunning {
			failures = 0
			continue
		}

		health := runCloudflareHealthCheck()
		d.mu.Lock()
		d.lastCloudflareHealth = &health
		d.mu.Unlock()
		d.refresh(currentState(), true)

		if !cloudflareHealthNeedsAutoRecovery(health) {
			failures = 0
			continue
		}
		failures++
		if failures < cloudflareWatchdogFailureThreshold {
			continue
		}
		if !lastRecovery.IsZero() && time.Since(lastRecovery) < cloudflareWatchdogRecoveryCooldown {
			continue
		}

		// 健康检查与真正执行恢复之间再次确认用户意图，避免恰好碰上手动停止。
		cfg, err = loadCloudflareDesktopConfig()
		if err != nil || !cfg.AutoRecover || !cfg.DesiredRunning {
			failures = 0
			continue
		}

		lastRecovery = time.Now()
		appendCloudflareWatchdogLog("连续 %d 次健康检查异常，开始按层级恢复；诊断：%s", failures, health.Diagnosis)
		if err := withLifecycle(func() error {
			// 等待其他操作完成后再次读取意图；已经手动停止时不再恢复。
			latest, err := loadCloudflareDesktopConfig()
			if err != nil {
				return err
			}
			if !latest.AutoRecover || !latest.DesiredRunning {
				return nil
			}
			return restartCloudflareAndMCP()
		}); err != nil {
			appendCloudflareWatchdogLog("自动恢复失败：%v", err)
			continue
		}
		appendCloudflareWatchdogLog("自动恢复操作完成，等待下一轮健康检查确认")
		failures = 0
		d.mu.Lock()
		d.lastCloudflareHealth = nil
		d.mu.Unlock()
		d.refresh(currentState(), true)
	}
}

func cloudflareStatusLabel(state cloudflareState) string {
	switch state.Status {
	case cloudflareStatusRunning:
		if state.Mode != "" {
			return fmt.Sprintf("状态：运行中 · %s · pid=%d", state.Mode, state.PID)
		}
		return fmt.Sprintf("状态：运行中 · pid=%d", state.PID)
	case cloudflareStatusStarting:
		return "状态：启动中"
	case cloudflareStatusError:
		return "状态：异常"
	default:
		if !state.Software.Installed {
			return "状态：已停止 · cloudflared 未安装"
		}
		return "状态：已停止"
	}
}

func (d *desktopApp) cloudflareHealthLabel(state cloudflareState) string {
	d.mu.Lock()
	health := d.lastCloudflareHealth
	d.mu.Unlock()
	if health != nil {
		if health.OK {
			return "健康状态：正常"
		}
		return "健康状态：异常"
	}
	if !state.Software.Installed {
		return "健康状态：未安装 cloudflared"
	}
	if !cloudflareManaged(state) {
		return "健康状态：Tunnel 未运行"
	}
	return "健康状态：未检查"
}

// refresh 把最新状态同步到托盘图标、提示文字和菜单。
// force 用于首次装配；其余时候状态没变就不重建菜单，避免菜单在用户眼皮底下闪。
func (d *desktopApp) refresh(state serviceState, force bool) {
	cloudflare := currentCloudflareState()
	d.mu.Lock()
	serviceChanged := d.last != state
	cloudflareChanged := d.lastCloudflare != cloudflare
	changed := force || serviceChanged || cloudflareChanged
	if serviceChanged || cloudflareChanged {
		d.lastCloudflareHealth = nil
	}
	d.last = state
	d.lastCloudflare = cloudflare
	d.mu.Unlock()
	if !changed {
		return
	}

	switch state.Status {
	case statusRunning:
		d.tray.SetIcon(iconRunning)
		d.tray.SetTooltip(fmt.Sprintf("MCPX 运行中 · %s", state.Addr))
	case statusStarting:
		d.tray.SetIcon(iconStarting)
		d.tray.SetTooltip("MCPX 启动中")
	case statusConflict:
		d.tray.SetIcon(iconStarting)
		d.tray.SetTooltip(fmt.Sprintf("MCPX 端口 %s 被其他进程占用", state.Addr))
	default:
		d.tray.SetIcon(iconStopped)
		d.tray.SetTooltip("MCPX 已停止")
	}
	d.tray.SetMenu(d.buildMenu(state, cloudflare))
}

func (d *desktopApp) buildMenu(state serviceState, cloudflare cloudflareState) *application.Menu {
	menu := d.app.NewMenu()

	status := menu.Add(statusLabel(state))
	status.SetEnabled(false)
	menu.AddSeparator()

	manageCloudflareWithMCPX := false
	if cfg, err := loadCloudflareDesktopConfig(); err == nil {
		manageCloudflareWithMCPX = cfg.ManageWithMCPX
	}

	start := menu.Add("启动 MCPX")
	start.OnClick(func(*application.Context) { d.runServiceAction(startMCPXStack) })
	if manageCloudflareWithMCPX {
		// 联动模式下，本地 Runtime 或 Tunnel 任一未运行都允许补齐整套服务。
		start.SetEnabled(state.Status != statusConflict &&
			!(state.Status == statusRunning && cloudflareManaged(cloudflare)))
	} else {
		// 手动 Tunnel 模式下，MCPX 菜单只管理本地 Runtime。
		start.SetEnabled(state.Status == statusStopped)
	}

	stop := menu.Add("停止 MCPX")
	stop.OnClick(func(*application.Context) { d.runServiceAction(stopMCPXStack) })
	if manageCloudflareWithMCPX {
		stop.SetEnabled(managed(state) || cloudflareManaged(cloudflare))
	} else {
		stop.SetEnabled(managed(state))
	}

	restart := menu.Add("重启 MCPX")
	restart.OnClick(func(*application.Context) { d.runServiceAction(restartMCPXStack) })
	if manageCloudflareWithMCPX {
		restart.SetEnabled(managed(state) || cloudflareManaged(cloudflare))
	} else {
		restart.SetEnabled(managed(state))
	}

	menu.AddSeparator()
	cloudflareMenu := menu.AddSubmenu("Cloudflare Tunnel")
	cloudflareStatus := cloudflareMenu.Add(cloudflareStatusLabel(cloudflare))
	cloudflareStatus.SetEnabled(false)

	healthStatus := cloudflareMenu.Add(d.cloudflareHealthLabel(cloudflare))
	healthStatus.SetEnabled(false)

	if cloudflare.PublicMCPURL != "" {
		publicURL := cloudflareMenu.Add("公网 MCP · " + cloudflare.PublicMCPURL)
		publicURL.SetEnabled(false)
	}

	cloudflareMenu.AddSeparator()
	startCloudflare := cloudflareMenu.Add("一键启动公网 MCP（MCPX + Tunnel）")
	startCloudflare.OnClick(func(*application.Context) {
		d.runCloudflareAction(func() error {
			_, err := startCloudflareTunnel()
			return err
		})
	})
	startCloudflare.SetEnabled(cloudflare.Software.Installed && !cloudflareManaged(cloudflare))

	stopCloudflare := cloudflareMenu.Add("停止 Cloudflare Tunnel")
	stopCloudflare.OnClick(func(*application.Context) { d.runCloudflareAction(stopCloudflareTunnel) })
	stopCloudflare.SetEnabled(cloudflareManaged(cloudflare))

	healthCheck := cloudflareMenu.Add("运行健康检查")
	healthCheck.OnClick(func(*application.Context) { d.refreshCloudflareHealth() })

	copyPublicURL := cloudflareMenu.Add("复制公网 MCP URL")
	copyPublicURL.OnClick(func(*application.Context) {
		d.app.Clipboard.SetText(cloudflare.PublicMCPURL)
	})
	copyPublicURL.SetEnabled(cloudflare.PublicMCPURL != "")

	menu.AddSeparator()
	menu.Add("打开主窗口").OnClick(func(*application.Context) { d.showWindow() })
	menu.Add("复制端点地址").OnClick(func(*application.Context) {
		d.app.Clipboard.SetText(state.Endpoint)
	})
	menu.Add("打开运行时目录").OnClick(func(*application.Context) {
		if state.HomeDir != "" {
			_ = d.app.Browser.OpenFile(state.HomeDir)
		}
	})
	menu.Add("打开日志").OnClick(func(*application.Context) {
		if state.LogPath != "" {
			_ = d.app.Browser.OpenFile(state.LogPath)
		}
	})

	menu.AddSeparator()
	autostartOn, err := d.app.Autostart.IsEnabled()
	if err == nil {
		item := menu.AddCheckbox("开机自启", autostartOn)
		item.OnClick(func(ctx *application.Context) {
			d.toggleAutostart(ctx.ClickedMenuItem().Checked())
		})
	}

	menu.AddSeparator()
	// 只退出托盘，后台服务不受影响。
	menu.Add("退出托盘").OnClick(func(*application.Context) { d.app.Quit() })
	return menu
}

// managed 表示当前进程是由托盘/CLI 的 daemon 状态文件跟踪的，可以被停止。
// 端口冲突场景下的占用者不属于此列，我们不去动别人的进程。
func managed(state serviceState) bool {
	return state.Status == statusRunning || state.Status == statusStarting
}

func cloudflareManaged(state cloudflareState) bool {
	return state.Status == cloudflareStatusRunning || state.Status == cloudflareStatusStarting
}

func statusLabel(state serviceState) string {
	switch state.Status {
	case statusRunning:
		return fmt.Sprintf("运行中 · pid=%d · %s", state.PID, state.Addr)
	case statusStarting:
		return fmt.Sprintf("启动中 · pid=%d · 端口未就绪", state.PID)
	case statusConflict:
		return fmt.Sprintf("端口 %s 被其他进程占用", state.Addr)
	default:
		return "已停止"
	}
}

// runServiceAction 在后台执行启停操作。启停会等待状态稳定（最长 10s），
// 直接在菜单回调里同步执行会卡住整个 UI。
func (d *desktopApp) runServiceAction(action func() error) {
	go func() {
		if err := withLifecycle(action); err != nil {
			d.app.Dialog.Error().SetMessage(err.Error()).Show()
		}
		d.refresh(currentState(), true)
	}()
}

// runCloudflareAction 与服务启停一致放到后台执行。Quick Tunnel 启动时可能要等
// cloudflared 返回公网 URL，不能阻塞系统托盘的菜单线程。
func (d *desktopApp) runCloudflareAction(action func() error) {
	go func() {
		if err := withLifecycle(action); err != nil {
			d.app.Dialog.Error().SetMessage(err.Error()).Show()
		}
		d.mu.Lock()
		d.lastCloudflareHealth = nil
		d.mu.Unlock()
		d.refresh(currentState(), true)
	}()
}

// refreshCloudflareHealth 执行一次完整健康检查并把结果缓存在托盘菜单中。检查包含
// 公网 HTTP/OAuth 请求，必须异步执行；Tunnel 或服务状态改变后缓存会被清空。
func (d *desktopApp) refreshCloudflareHealth() {
	go func() {
		health := runCloudflareHealthCheck()
		d.mu.Lock()
		d.lastCloudflareHealth = &health
		d.mu.Unlock()
		d.refresh(currentState(), true)
	}()
}

func (d *desktopApp) toggleAutostart(enable bool) {
	var err error
	if enable {
		// 注册独立管理器自身；-tray 只驻留托盘，不启动 Runtime 或 Tunnel。
		err = d.app.Autostart.EnableWithOptions(application.AutostartOptions{
			Arguments: []string{"-" + trayOnlyFlag},
		})
	} else {
		err = d.app.Autostart.Disable()
	}
	if err != nil {
		d.app.Dialog.Error().SetMessage(fmt.Sprintf("设置开机自启失败：%v", err)).Show()
	}
	d.refresh(currentState(), true)
}
