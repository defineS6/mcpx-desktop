//go:build windows

package desktop

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// frontendHandler 发布模式只使用当前桌面协议的资源；开发模式显式代理本机 Vite 预览。
// 开发预览不生成发布资源，也不能替代正式前端打包。
func frontendHandler(args []string) (http.Handler, error) {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-dev-url=") {
			return developmentAssets(strings.TrimPrefix(arg, "-dev-url="))
		}
	}
	manifest, err := frontendAssets.ReadFile("frontend/dist/desktop-manifest.json")
	if err != nil {
		return nil, fmt.Errorf("前端资源尚未生成或仍为旧版；请先在 internal/desktop/frontend 完成 npm run build，再编译桌面程序。开发预览可使用 -dev-url=http://127.0.0.1:5173")
	}
	var contract struct {
		APIVersion int `json:"api_version"`
	}
	if json.Unmarshal(manifest, &contract) != nil || contract.APIVersion != 1 {
		return nil, fmt.Errorf("前端资源协议不匹配，请重新打包前端和桌面程序")
	}
	return application.BundledAssetFileServer(frontendAssets), nil
}

// developmentAssets 只允许显式选择回环 IP 的 HTTP 开发服务，拒绝远程页面访问本地管理接口。
func developmentAssets(raw string) (http.Handler, error) {
	target, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("开发预览地址无效：%w", err)
	}
	ip := net.ParseIP(target.Hostname())
	if target.Scheme != "http" || ip == nil || !ip.IsLoopback() || target.Port() == "" || target.User != nil || target.RawQuery != "" || target.Fragment != "" || (target.Path != "" && target.Path != "/") {
		return nil, fmt.Errorf("开发预览只允许 http://127.0.0.1:端口 或 http://[::1]:端口，不允许凭据和路径")
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "无法连接本机 Vite 开发服务", http.StatusBadGateway)
	}
	return proxy, nil
}
