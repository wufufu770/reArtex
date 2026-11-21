package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRoutesRegisteredWiring 是编排层接线的守门人：每个单元提供了注册函数，
// 但「注册函数存在」不等于「路由表里接上了」。这个用例逐条打过去，缺哪条报哪条。
//
// 用真实的 server 实例（taskHallTestServer 会起 Manager 并接好路由表），
// 而不是空壳 &Server{} —— 空壳没有 m 字段，handler 里的 s.pg(w) 会崩，
// 那样测的就不是「路由有没有接」而是「handler 能不能跑」。
//
// 判定依据是**响应体**而不是状态码：handler 自己也可能返回 404
// （比如「群组不存在」），那与「路由没注册」是两回事。ServeMux 未匹配时的
// 默认响应体是纯文本 "404 page not found"，而 handler 的 404 是 JSON ——
// 用这个差异才能把两件事分开。
func TestRoutesRegisteredWiring(t *testing.T) {
	s, _ := taskHallTestServer(t)
	mux := http.NewServeMux()
	s.RegisterTaskHallGroupRoutes(mux)
	s.RegisterTaskHallRoutes(mux)
	s.RegisterTaskHallQuotaRoutes(mux)
	RegisterEgressGovernanceRoutes(mux)
	s.registerTaskSteerRoutes(mux)          // 0.15.16 三档干预
	s.registerExecutionProfileRoutes(mux)   // 0.15.17 执行档位
	s.registerSelfEvolveRoutes(mux)         // 0.15.41 自进化可见面

	cases := []struct {
		method, path string
	}{
		{"GET", "/api/task-hall/groups"},
		{"POST", "/api/task-hall/tasks"},
		{"GET", "/api/task-hall/groups/1/quota"},
		{"GET", "/api/settings/egress-governance"},
		{"POST", "/api/tasks/1/steer"},
		{"GET", "/api/tasks/1/execution-profile"},
		{"PATCH", "/api/tasks/1/execution-profile"},
		{"GET", "/api/self-evolve/status"},
		{"GET", "/api/self-evolve/amber/items"},
		{"GET", "/api/self-evolve/amber/quarantine"},
		{"GET", "/api/self-evolve/amber/search?q=x"},
		{"GET", "/api/self-evolve/veto/exports"},
		{"GET", "/api/self-evolve/veto/exports/1"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if strings.Contains(rec.Body.String(), "404 page not found") {
			t.Errorf("路由未注册: %s %s → %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}
