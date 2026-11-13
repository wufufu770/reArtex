package server

// 小程序运行时反代（0.13.0 §四-B8 内嵌浏览器面板的数据面）。
//
// 需求：用户在小程序渗透页里【直接调试/点击】运行中的小程序页面——wxa-runner
// 把小程序编译成 Web 页面跑在本机端口上，把它反代到 artex 同源路径下，前端
// iframe 即可内嵌（客户端设备不需要访问服务器本机端口，同源无跨域）。
//
// 安全口径（SSRF 面）：
//   - 目标 URL 是【部署配置】（settings 键 miniprogram_runtime_url，操作者经 REST
//     设置），不是 agent 可控输入——agent 只能读不能改；
//   - scheme 必须 http/https；目标是【唯一固定目标】——路径由请求方拼接，host
//     恒等于配置值，不存在任意主机转发（与 WebFetch 的任意 URL 语义不同类）；
//   - 运行时（wxa-runner dev server）部署在 artex 同机，环回目标是部署形态的
//     必然形态（与 DB DSN、MCP stdio 的本机语义同类）；远端运行时同样支持
//     （配置成远端 http URL 即可，此时目标在受管网段由部署方授权）。

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
)

// mpRuntimeProxy 是运行时反代 handler（懒初始化 ReverseProxy：配置变更翻代）。
type mpRuntimeProxy struct {
	mu   sync.RWMutex
	url  *url.URL // 当前目标（nil = 未配置）
	proxy *httputil.ReverseProxy
}

var mpRuntime = &mpRuntimeProxy{}

// mpRuntimeSetURL 更新反代目标（REST 设置时调用；空串=清除）。
func mpRuntimeSetURL(raw string) error {
	raw = strings.TrimSpace(raw)
	mpRuntime.mu.Lock()
	defer mpRuntime.mu.Unlock()
	if raw == "" {
		mpRuntime.url = nil
		mpRuntime.proxy = nil
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errRuntimeScheme
	}
	mpRuntime.url = u
	mpRuntime.proxy = httputil.NewSingleHostReverseProxy(u)
	return nil
}

// mpRuntimeGetURL 读当前目标（前端状态面）。
func mpRuntimeGetURL() string {
	mpRuntime.mu.RLock()
	defer mpRuntime.mu.RUnlock()
	if mpRuntime.url == nil {
		return ""
	}
	return mpRuntime.url.String()
}

// handler 反代到运行时目标：前缀 /mp-runtime 剥离后转发（路径/查询原样拼接）。
func (p *mpRuntimeProxy) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.RLock()
		target, proxy := p.url, p.proxy
		p.mu.RUnlock()
		if target == nil || proxy == nil {
			http.Error(w, "小程序运行时未配置：先在「小程序渗透」页设置运行时地址（wxa-runner dev server）", http.StatusServiceUnavailable)
			return
		}
		single := *r.URL
		rel := strings.TrimPrefix(r.URL.Path, "/mp-runtime")
		single.Path = strings.TrimSuffix(target.Path, "/") + rel
		single.RawQuery = r.URL.RawQuery
		r2 := r.Clone(r.Context())
		r2.URL = &single
		// Host 交给目标（wxa-runner 的路由对 Host 不敏感，但保留 Target.Host 是
		// ReverseProxy 标准行为）。
		r2.Host = target.Host
		proxy.ServeHTTP(w, r2)
	})
}

// errRuntimeScheme 是运行时 URL 的 scheme 校验错误（仅 http/https）。
var errRuntimeScheme = &runtimeSchemeError{}

type runtimeSchemeError struct{}

func (e *runtimeSchemeError) Error() string {
	return "运行时地址仅支持 http/https"
}
