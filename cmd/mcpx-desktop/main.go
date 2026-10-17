// MCPX Desktop 仅启动管理界面，不包含 MCP Runtime。
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/defineS6/mcpx-desktop/internal/desktop"
)

var version = "0.1.0"

// main 解析管理器参数，启动失败时提供可见错误，不留下看不见的后台失败。
func main() {
	if err := run(); err != nil {
		showError(err.Error())
		os.Exit(1)
	}
}

// run 支持托盘驻留、隔离运行目录和一次性内核路径覆盖；不自动启动任何服务。
func run() error {
	flags := flag.NewFlagSet("mcpx-desktop", flag.ContinueOnError)
	tray := flags.Bool("tray", false, "只驻留系统托盘，不打开主窗口")
	home := flags.String("home", "", "覆盖 MCPX_HOME 运行目录")
	runtime := flags.String("runtime", "", "本次进程使用的外部 mcpx.exe 路径（未保存路径时生效）")
	devURL := flags.String("dev-url", "", "显式使用本机 Vite 开发预览地址；不生成发布资源")
	inspectPort := flags.Int("inspect-port", 0, "仅开发预览时启用本机 WebView 调试端口")
	printVersion := flags.Bool("version", false, "输出管理器版本")
	if err := flags.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("不支持的位置参数：%v", flags.Args())
	}
	if *printVersion {
		fmt.Printf("mcpx-desktop %s\n", version)
		return nil
	}
	if *home != "" {
		if err := os.Setenv("MCPX_HOME", *home); err != nil {
			return err
		}
	}
	if *runtime != "" {
		if err := os.Setenv("MCPX_RUNTIME", *runtime); err != nil {
			return err
		}
	}
	args := []string{}
	if *devURL != "" {
		args = append(args, "-dev-url="+*devURL)
	}
	if *inspectPort != 0 {
		if *devURL == "" || *inspectPort < 1 || *inspectPort > 65535 {
			return fmt.Errorf("调试端口仅允许在开发预览中使用，范围为 1-65535")
		}
		args = append(args, fmt.Sprintf("-inspect-port=%d", *inspectPort))
	}
	if *tray {
		args = append(args, "-tray")
	}
	return desktop.Run(args)
}
