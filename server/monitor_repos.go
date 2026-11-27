// monitor_repos.go：Monitor 仓库配置 CRUD API（B-09C）。
//
// 路由面（全部在 requireAuth 内，server.go 注册）：
//
//	GET    /api/monitor/repos        列配置（?enabled=1 只列启用）
//	POST   /api/monitor/repos        新建/更新（(repo_url,branch) 幂等 upsert）
//	GET    /api/monitor/repos/{id}   查单条
//	PATCH  /api/monitor/repos/{id}   启停（{enabled:bool}）
//	DELETE /api/monitor/repos/{id}   删除（findings 挂靠时 RESTRICT → 409）
//
// 安全纪律（卡要求逐条）：
//   - 鉴权复用 requireAuth（单租户 JWT；无用户身份 claims，操作者固定
//     Actor="operator"，与 miniprogram_api 同款口径，如实申报）；
//   - SSRF 策略在存储形态校验（db 层：https + owner/repo 两段 + 拒 userinfo/
//     查询串/片段）之上加 API 层策略：拒绝 localhost 变体与私网/环回/链路本地
//     IP 字面量——DNS 级解析 SSRF（hostname 指向内网）不在此层，属运行环境
//     出网管控层（如实申报边界；hostname 形态的 GHE 是设计目标）；
//   - auth_ref 只收引用串：token 明文形态（ghp_ 前缀族/经典 PAT）在 db 层
//     已点名拒绝；本层请求体根本没有 token 字段（结构性不收）；
//   - 响应只回显 auth_ref 引用串本身（操作者需要看到仓库指向哪个凭据引用），
//     绝无底层 secret——引用不是凭据；
//   - 写动作全部进审计链（AppendAuditEvent best-effort，凭据值/底层 secret
//     绝不进审计载荷；auth_ref 引用串进载荷=审计「指向哪个凭据」的事实）；
//   - 错误面只回业务校验消息/固定文案，无 stack、无内部错误串直透（数据源
//     故障统一 errDataSourceUnavailable）。
package server

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// monitorMaxBody 是配置写请求体上限（B-07B dispatch 同款 64KB）。
const monitorMaxBody = 64 << 10

// monitorRepoDTO 是 API 响应形态（auth_ref 是引用串非凭据，如实回显）。
type monitorRepoDTO struct {
	ID            int64  `json:"id"`
	RepoURL       string `json:"repo_url"`
	Branch        string `json:"branch"`
	GitHubBaseURL string `json:"github_base_url"`
	AuthRef       string `json:"auth_ref"`
	Enabled       bool   `json:"enabled"`
	LastSeenSHA   string `json:"last_seen_sha"`
	CreatedBy     string `json:"created_by"`
	CreatedAt     string `json:"created_at"`
}

func monitorRepoToDTO(r *db.MonitorRepo) monitorRepoDTO {
	return monitorRepoDTO{
		ID: r.ID, RepoURL: r.RepoURL, Branch: r.Branch,
		GitHubBaseURL: r.GitHubBaseURL, AuthRef: r.AuthRef,
		Enabled: r.Enabled, LastSeenSHA: r.LastSeenSHA,
		CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// monitorAPIHostPolicy 是 API 层 SSRF 主机策略：拒绝 localhost 变体与
// 非公网 IP 字面量（环回/私网/链路本地/CGNAT/未指定）。hostname 形态放行
// （GHE 自建域名是设计目标；请求时的 DNS/拨号级校验在 monitor_github.go
// 客户端层闭环——preflight + 拨号闸双层）。IP 判定走 monitorNetGuardIP 缝隙
// （生产恒为 isPublicIP；httptest 本地环回测试缝隙在测试进程内生效）。
func monitorAPIHostPolicy(hostport string) error {
	host := hostport
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host, "]") {
		host = host[:i] // 剥端口（IPv6 字面量带 [] 不剥——v6 字面量本身会被下面的 IP 闸处理）
	}
	host = strings.Trim(host, "[]")
	host = strings.TrimSuffix(host, ".") // FQDN 尾点归一（localhost. ≠ localhost 的绕过面封堵）
	low := strings.ToLower(host)
	if low == "localhost" || strings.HasSuffix(low, ".localhost") ||
		strings.HasSuffix(low, ".local") || strings.HasSuffix(low, ".internal") {
		return errMonitorHostDenied
	}
	if ip := net.ParseIP(host); ip != nil {
		if !monitorNetGuardIP(ip) {
			return errMonitorHostDenied
		}
	}
	return nil
}

var errMonitorHostDenied = constErr("monitor: host 指向 localhost/私网/环回地址，已被 SSRF 策略拒绝")

// constErr 是静态错误类型（errors.New 的可复用形态）。
type constErr string

func (e constErr) Error() string { return string(e) }

// isPublicIP 判断是否公网 IP（非环回/私网/链路本地/CGNAT/未指定/组播）。
func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		// 100.64.0.0/10 CGNAT 段不在 net.IP 的 IsPrivate 里，点名拒绝。
		if v4[0] == 100 && v4[1] >= 64 && v4[1] < 128 {
			return false
		}
		if v4[0] == 0 { // 0.0.0.0/8
			return false
		}
	}
	return true
}

// monitorUpsertReq 是 POST 请求体。没有 token 字段——结构性不收 token 明文。
type monitorUpsertReq struct {
	RepoURL       string  `json:"repo_url"`
	Branch        string  `json:"branch"`
	GitHubBaseURL string  `json:"github_base_url"`
	AuthRef       string  `json:"auth_ref"`
	Enabled       *bool   `json:"enabled"`
	CreatedBy     *string `json:"created_by"`
}

// monitorRepoList 处理 GET /api/monitor/repos。
func (s *Server) monitorRepoList(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	enabledOnly := r.URL.Query().Get("enabled") == "1"
	repos, err := pg.ListMonitorRepos(enabledOnly)
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	out := make([]monitorRepoDTO, 0, len(repos))
	for _, rp := range repos {
		out = append(out, monitorRepoToDTO(rp))
	}
	writeJSON(w, 200, map[string]any{"repos": out, "count": len(out)})
}

// monitorRepoUpsert 处理 POST /api/monitor/repos（(repo_url,branch) 幂等）。
func (s *Server) monitorRepoUpsert(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req monitorUpsertReq
	r.Body = http.MaxBytesReader(w, r.Body, monitorMaxBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON 或超过大小上限")
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	createdBy := "operator"
	if req.CreatedBy != nil && strings.TrimSpace(*req.CreatedBy) != "" {
		createdBy = strings.TrimSpace(*req.CreatedBy)
	}
	// API 层 SSRF 策略（存储层形态校验在 db.UpsertMonitorRepo 内再走一道）。
	for _, u := range []string{req.RepoURL, req.GitHubBaseURL} {
		if u == "" {
			continue
		}
		if err := monitorAPIURLPolicy(u); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	id, err := pg.UpsertMonitorRepo(req.RepoURL, req.Branch, req.GitHubBaseURL, req.AuthRef, createdBy, enabled)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rp, err := pg.GetMonitorRepo(id)
	if err != nil || rp == nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	// 审计链（best-effort；凭据值绝不进载荷——本请求体也没有凭据字段）。
	auditBody, _ := json.Marshal(map[string]any{"id": id, "repo_url": rp.RepoURL,
		"branch": rp.Branch, "enabled": rp.Enabled, "auth_ref_set": rp.AuthRef != "", "via": "rest"})
	if _, err := pg.AppendAuditEvent(db.AuditEvent{
		Actor: "operator", ToolName: "monitor", Action: "monitor-repo-save",
		Reason:  "repo=" + rp.RepoURL + " branch=" + rp.Branch,
		Payload: auditBody, Source: "monitor-api",
	}); err != nil {
		log.Printf("[monitor] 配置保存审计链写入失败（忽略）: %v", err)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "repo": monitorRepoToDTO(rp)})
}

// monitorAPIURLPolicy 组合 API 层策略：https 形态（由 db 层复核，这里先做
// host 策略检查需要的宽松解析）+ host 白单闸。
func monitorAPIURLPolicy(raw string) error {
	u := strings.TrimSpace(raw)
	if u == "" {
		return nil // 可选字段（base_url 缺省走默认）
	}
	if !strings.HasPrefix(u, "https://") {
		return constErr("monitor: URL 必须是 https://")
	}
	rest := strings.TrimPrefix(u, "https://")
	host := rest
	if i := strings.Index(rest, "/"); i >= 0 {
		host = rest[:i]
	}
	return monitorAPIHostPolicy(host)
}

// monitorRepoGet 处理 GET /api/monitor/repos/{id}。
func (s *Server) monitorRepoGet(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "id 非法")
		return
	}
	rp, err := pg.GetMonitorRepo(id)
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	if rp == nil {
		writeErr(w, 404, "配置不存在")
		return
	}
	writeJSON(w, 200, monitorRepoToDTO(rp))
}

// monitorRepoEnable 处理 PATCH /api/monitor/repos/{id}（{enabled:bool}）。
func (s *Server) monitorRepoEnable(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "id 非法")
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, monitorMaxBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enabled == nil {
		writeErr(w, 400, "请求体需要 {\"enabled\":bool}")
		return
	}
	if err := pg.SetMonitorRepoEnabled(id, *req.Enabled); err != nil {
		if strings.Contains(err.Error(), "不存在") {
			writeErr(w, 404, "配置不存在")
			return
		}
		writeErr(w, 400, err.Error())
		return
	}
	auditBody, _ := json.Marshal(map[string]any{"id": id, "enabled": *req.Enabled, "via": "rest"})
	if _, err := pg.AppendAuditEvent(db.AuditEvent{
		Actor: "operator", ToolName: "monitor", Action: "monitor-repo-enable",
		Reason:  "id=" + strconv.FormatInt(id, 10),
		Payload: auditBody, Source: "monitor-api",
	}); err != nil {
		log.Printf("[monitor] 启停审计链写入失败（忽略）: %v", err)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "id": id, "enabled": *req.Enabled})
}

// monitorRepoDelete 处理 DELETE /api/monitor/repos/{id}。findings 挂靠时
// 存储层 RESTRICT 拒绝 → 409（处置历史绝不隐式清空，操作者需先显式处置）。
func (s *Server) monitorRepoDelete(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "id 非法")
		return
	}
	rp, err := pg.GetMonitorRepo(id)
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	if rp == nil {
		writeErr(w, 404, "配置不存在")
		return
	}
	if err := pg.DeleteMonitorRepo(id); err != nil {
		if strings.Contains(err.Error(), "findings") || strings.Contains(err.Error(), "foreign key") {
			writeErr(w, 409, "存在监测发现挂靠：请先显式处置（reproduced/ignored）并删除 findings，再删除仓库配置")
			return
		}
		writeErr(w, 400, err.Error())
		return
	}
	auditBody, _ := json.Marshal(map[string]any{"id": id, "repo_url": rp.RepoURL, "branch": rp.Branch, "via": "rest"})
	if _, err := pg.AppendAuditEvent(db.AuditEvent{
		Actor: "operator", ToolName: "monitor", Action: "monitor-repo-delete",
		Reason:  "repo=" + rp.RepoURL + " branch=" + rp.Branch,
		Payload: auditBody, Source: "monitor-api",
	}); err != nil {
		log.Printf("[monitor] 删除审计链写入失败（忽略）: %v", err)
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
