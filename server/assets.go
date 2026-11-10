package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

// companyScopeInputs accepts both the new [{kind,value}] contract and the
// historical ["example.com","203.0.113.10"] contract.
type companyScopeInputs []db.ScopeInput

const maxCompanyMutationBodyBytes = 2 << 20

func decodeCompanyMutationRequest(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxCompanyMutationBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(value); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "请求正文过大")
		} else {
			writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		}
		return false
	}
	return true
}

func (items *companyScopeInputs) UnmarshalJSON(data []byte) error {
	var rawItems []json.RawMessage
	if err := json.Unmarshal(data, &rawItems); err != nil {
		return err
	}
	out := make([]db.ScopeInput, 0, len(rawItems))
	for i, raw := range rawItems {
		var legacy string
		if err := json.Unmarshal(raw, &legacy); err == nil {
			out = append(out, db.ScopeInput{Value: legacy})
			continue
		}
		var structured db.ScopeInput
		if err := json.Unmarshal(raw, &structured); err != nil {
			return fmt.Errorf("scope[%d] must be a string or {kind,value}", i)
		}
		out = append(out, structured)
	}
	*items = out
	return nil
}

// assetStore returns the asset store.
func (s *Server) assetStore() *db.AssetStore {
	if s.m.pg == nil {
		return nil
	}
	return s.m.pg.Assets()
}

// companyStore returns the company store.
func (s *Server) companyStore() *db.CompanyStore {
	if s.m.pg == nil {
		return nil
	}
	return s.m.pg.Companies()
}

// =====================================================================
// GET /api/companies
// =====================================================================

func (s *Server) listCompanies(w http.ResponseWriter, r *http.Request) {
	cs := s.companyStore()
	if cs == nil {
		writeErr(w, 503, "database unavailable")
		return
	}
	companies, err := cs.ListCompanies()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, companies)
}

// =====================================================================
// POST /api/companies
// =====================================================================

func (s *Server) createCompany(w http.ResponseWriter, r *http.Request) {
	cs := s.companyStore()
	if cs == nil {
		writeErr(w, 503, "database unavailable")
		return
	}
	var req struct {
		Name  string             `json:"name"`
		Logo  string             `json:"logo"`
		Scope companyScopeInputs `json:"scope"`
	}
	if !decodeCompanyMutationRequest(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeErr(w, 400, "name required")
		return
	}
	if err := db.ValidateCompanyScopeInputBounds(req.Scope); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id, added, skipped, invalid, scopeErrs, err := cs.CreateCompanyWithScope(req.Name, req.Logo, req.Scope, "api")
	if err != nil {
		if errors.Is(err, db.ErrCompanyNameConflict) {
			writeErr(w, http.StatusConflict, "企业名称已存在")
			return
		}
		var validationErr *db.CompanyScopeValidationError
		if errors.As(err, &validationErr) {
			writeErr(w, http.StatusBadRequest, validationErr.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := map[string]any{
		"id":            id,
		"created":       true,
		"scope_added":   added,
		"scope_skipped": skipped,
		"scope_invalid": invalid,
	}
	if len(scopeErrs) > 0 {
		out["scope_errors"] = scopeErrs
	}
	writeJSON(w, 201, out)
}

// =====================================================================
// GET /api/companies/{id}
// =====================================================================

func (s *Server) getCompany(w http.ResponseWriter, r *http.Request) {
	cs := s.companyStore()
	if cs == nil {
		writeErr(w, 503, "database unavailable")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid id")
		return
	}
	c, err := cs.GetCompany(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if c == nil {
		writeErr(w, 404, "company not found")
		return
	}
	scope, err := cs.GetScope(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"company": c, "scope": scope})
}

// =====================================================================
// POST /api/companies/{id}/scope
// =====================================================================

func (s *Server) addCompanyScope(w http.ResponseWriter, r *http.Request) {
	cs := s.companyStore()
	if cs == nil {
		writeErr(w, 503, "database unavailable")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid company id")
		return
	}
	var req struct {
		Scope  companyScopeInputs `json:"scope"`
		Reason string             `json:"reason"`
		Reset  bool               `json:"reset"` // if true, replace existing scope
	}
	if !decodeCompanyMutationRequest(w, r, &req) {
		return
	}
	if err := db.ValidateCompanyScopeInputBounds(req.Scope); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var added, skipped, invalid int
	var errs []string
	var mutationErr error
	if req.Reset {
		added, invalid, errs, mutationErr = cs.UpdateScopeInputsChecked(id, req.Scope, req.Reason)
	} else {
		added, skipped, invalid, errs, mutationErr = cs.AddScopeInputsChecked(id, req.Scope, req.Reason)
	}
	if mutationErr != nil {
		if errors.Is(mutationErr, db.ErrCompanyNotFound) {
			writeErr(w, http.StatusNotFound, "company not found")
			return
		}
		var validationErr *db.CompanyScopeValidationError
		if errors.As(mutationErr, &validationErr) {
			writeErr(w, http.StatusBadRequest, validationErr.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, mutationErr.Error())
		return
	}
	out := map[string]any{
		"added":   added,
		"skipped": skipped,
		"invalid": invalid,
	}
	if len(errs) > 0 {
		out["errors"] = errs
	}
	// Reported separately from errors: this is pre-existing bad data, not a fault
	// in the submitted rules, but the operator still has to see it or those assets
	// look like the IP/CIDR rules simply never match. A failure to build the
	// warning must not fail the scope write that already committed.
	if warning, err := cs.MalformedIPAssetWarning(); err == nil && warning != "" {
		out["warnings"] = []string{warning}
	}
	writeJSON(w, 200, out)
}

// =====================================================================
// DELETE /api/companies/{id}
// =====================================================================

func (s *Server) deleteCompany(w http.ResponseWriter, r *http.Request) {
	cs := s.companyStore()
	if cs == nil {
		writeErr(w, 503, "database unavailable")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid id")
		return
	}
	var req struct {
		DeleteAssets bool `json:"delete_assets"`
	}
	// The body is optional. Only an actual empty body is ignored; malformed or
	// trailing JSON is a client error.
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&req); err != nil {
		if !errors.Is(err, io.EOF) {
			writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
	} else {
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			if err == nil {
				writeErr(w, http.StatusBadRequest, "invalid JSON: multiple values")
			} else {
				writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			}
			return
		}
	}

	assetsDeleted, err := s.m.DeleteCompanyWithAssets(id, req.DeleteAssets)
	if err != nil {
		if errors.Is(err, db.ErrCompanyNotFound) {
			writeErr(w, http.StatusNotFound, "company not found")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": 1, "assets_deleted": assetsDeleted})
}

// =====================================================================
// POST /api/companies/reattribute
// =====================================================================

func (s *Server) reattribute(w http.ResponseWriter, r *http.Request) {
	cs := s.companyStore()
	if cs == nil {
		writeErr(w, 503, "database unavailable")
		return
	}
	if err := cs.RecomputeAttribution(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// =====================================================================
// GET /api/assets
// =====================================================================

const (
	defaultAssetPageSize = 50
	maxAssetPageSize     = 200
)

func (s *Server) listAssets(w http.ResponseWriter, r *http.Request) {
	as := s.assetStore()
	if as == nil {
		writeErr(w, 503, "database unavailable")
		return
	}
	q := r.URL.Query()
	typ := q.Get("type")
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	if limit <= 0 {
		limit = defaultAssetPageSize
	} else if limit > maxAssetPageSize {
		limit = maxAssetPageSize
	}
	if offset < 0 {
		offset = 0
	}

	assets := []*db.Asset{}
	var err error
	// total is the full match count ignoring limit/offset. Count first so an offset
	// beyond the last row can return an empty page without an expensive scan.
	total := 0

	if dsl := q.Get("dsl"); dsl != "" {
		if err := db.ValidateDSL(dsl); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		// task_id scopes the DSL search to a task's assets (the task detail
		// "测试资产" search); 0 means the global asset view.
		taskID, _ := strconv.ParseInt(q.Get("task_id"), 10, 64)
		total, err = as.CountDSL(dsl, typ, taskID)
		if err == nil && offset < total {
			assets, err = as.QueryDSL(dsl, typ, taskID, limit, offset)
		}
	} else {
		companyID, _ := strconv.ParseInt(q.Get("company_id"), 10, 64)
		taskID, _ := strconv.ParseInt(q.Get("task_id"), 10, 64)
		switch {
		case companyID > 0:
			total, err = as.CountByCompany(companyID, typ)
			if err == nil && offset < total {
				assets, err = as.QueryByCompany(companyID, typ, limit, offset)
			}
		case taskID > 0:
			total, err = as.CountByTask(taskID, typ)
			if err == nil && offset < total {
				assets, err = as.QueryByTask(taskID, typ, limit, offset)
			}
		default:
			if typ == "" {
				typ = "root_domain"
			}
			total, err = as.CountByType(typ)
			if err == nil && offset < total {
				assets, err = as.QueryByType(typ, limit, offset)
			}
		}
	}

	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"count":  len(assets),
		"total":  total,
		"assets": assets,
	})
}

// =====================================================================
// GET /api/assets/counts
// =====================================================================

func (s *Server) assetCounts(w http.ResponseWriter, r *http.Request) {
	as := s.assetStore()
	if as == nil {
		writeErr(w, 503, "database unavailable")
		return
	}
	var counts map[string]int
	var err error
	if taskID, _ := strconv.ParseInt(r.URL.Query().Get("task_id"), 10, 64); taskID > 0 {
		counts, err = as.CountsByTypeForTask(taskID)
	} else {
		counts, err = as.CountsByType()
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, counts)
}

// =====================================================================
// DELETE /api/assets
// =====================================================================

func (s *Server) deleteAssets(w http.ResponseWriter, r *http.Request) {
	as := s.assetStore()
	if as == nil {
		writeErr(w, 503, "database unavailable")
		return
	}
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if len(req.IDs) == 0 {
		writeErr(w, 400, "ids required")
		return
	}
	deleted, err := as.DeleteByIDs(req.IDs)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": deleted})
}

// =====================================================================
// POST /api/assets
// =====================================================================

func (s *Server) insertAssets(w http.ResponseWriter, r *http.Request) {
	as := s.assetStore()
	if as == nil {
		writeErr(w, 503, "database unavailable")
		return
	}
	var req struct {
		TaskID int64 `json:"task_id"`
		Assets []struct {
			Type string `json:"type"`
			// root_domain / subdomain
			Domain      string   `json:"domain"`
			ICP         string   `json:"icp"`
			RecordType  string   `json:"record_type"`
			RecordValue []string `json:"record_value"`
			// ip
			IP           string           `json:"ip"`
			BoundDomains []string         `json:"bound_domains"`
			OpenPorts    []db.PortService `json:"open_ports"`
			// app
			AppName     string `json:"app_name"`
			BundleID    string `json:"bundle_id"`
			Category    string `json:"category"`
			Description string `json:"description"`
			AppICP      string `json:"app_icp"`
			// service http
			URL           string           `json:"url"`
			Technologies  []string         `json:"technologies"`
			StatusCode    *int             `json:"status_code"`
			ContentLength *int64           `json:"content_length"`
			PageTitle     string           `json:"page_title"`
			FaviconMMH3   string           `json:"favicon_mmh3"`
			Auth          []map[string]any `json:"auth"`
			ServiceIP     string           `json:"service_ip"`
			// service other
			Port        int    `json:"port"`
			ServiceName string `json:"service_name"`
			// endpoint
			Method string           `json:"method"`
			Params []map[string]any `json:"params"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid JSON: "+err.Error())
		return
	}

	type result struct {
		Index int    `json:"index"`
		ID    int64  `json:"id"`
		Type  string `json:"type"`
	}
	type errEntry struct {
		Index int    `json:"index"`
		Error string `json:"error"`
	}
	var results []result
	var errs []errEntry
	var enrichJobs []platformEnrichJob // W7：入库成功的域名/URL，出循环后统一补全

	for i, a := range req.Assets {
		var id int64
		var err error
		switch a.Type {
		case "root_domain":
			id, err = as.UpsertRootDomain(db.UpsertRootDomainReq{Domain: a.Domain, ICP: a.ICP, TaskID: req.TaskID})
		case "ip":
			id, err = as.UpsertIP(db.UpsertIPReq{IP: a.IP, BoundDomains: a.BoundDomains, OpenPorts: a.OpenPorts, TaskID: req.TaskID})
		case "subdomain":
			id, err = as.UpsertSubdomain(db.UpsertSubdomainReq{Domain: a.Domain, RecordType: a.RecordType, RecordValue: a.RecordValue, ICP: a.ICP, TaskID: req.TaskID})
		case "app":
			id, err = as.UpsertApp(db.UpsertAppReq{Name: a.AppName, BundleID: a.BundleID, Category: a.Category, Description: a.Description, ICP: a.AppICP, TaskID: req.TaskID})
		case "service":
			if a.URL != "" {
				svcIP := a.ServiceIP
				if svcIP == "" {
					svcIP = a.IP
				}
				id, err = as.UpsertHTTPService(db.UpsertHTTPServiceReq{URL: a.URL, Technologies: a.Technologies, StatusCode: a.StatusCode, ContentLength: a.ContentLength, PageTitle: a.PageTitle, FaviconMMH3: a.FaviconMMH3, Auth: a.Auth, IP: svcIP, TaskID: req.TaskID})
			} else {
				id, err = as.UpsertOtherService(db.UpsertOtherServiceReq{Domain: a.Domain, IP: a.IP, Port: a.Port, ServiceName: a.ServiceName, Auth: a.Auth, TaskID: req.TaskID})
			}
		case "endpoint":
			svcIP := a.ServiceIP
			if svcIP == "" {
				svcIP = a.IP
			}
			id, err = as.UpsertEndpoint(db.UpsertEndpointReq{URL: a.URL, Method: a.Method, Params: a.Params, IP: svcIP, TaskID: req.TaskID})
		default:
			errs = append(errs, errEntry{Index: i, Error: "unknown type: " + a.Type})
			continue
		}
		if err != nil {
			errs = append(errs, errEntry{Index: i, Error: err.Error()})
			continue
		}
		if req.TaskID > 0 {
			_ = as.SetTaskAssetSource(req.TaskID, id, "api", "通过资产 API 登记", nil)
		}
		results = append(results, result{Index: i, ID: id, Type: a.Type})
		// W7（方案 v10 §6）：入库成功即收集补全作业——新域名走 DNS 解析、带 URL 的走 HTTP 探测。
		switch {
		case (a.Type == "root_domain" || a.Type == "subdomain") && a.Domain != "":
			enrichJobs = append(enrichJobs, platformEnrichJob{id: id, domain: a.Domain})
		case a.URL != "" && (a.Type == "service" || a.Type == "endpoint"):
			enrichJobs = append(enrichJobs, platformEnrichJob{id: id, url: a.URL})
		case a.Type == "service" && a.Domain != "":
			enrichJobs = append(enrichJobs, platformEnrichJob{id: id, domain: a.Domain})
		}
	}
	// W7（方案 v10 §6/§15）：平台侧入库出口的补全 + activity 留痕（best-effort，不影响响应）。
	s.enrichPlatformAssets(req.TaskID, enrichJobs)
	writeJSON(w, 200, map[string]any{"results": results, "errors": errs})
}

// platformEnrichJob 是一条待补全的入库资产：id 是触发补全的资产 id（enrich 入队的
// 挂靠键），domain/url 二选一或同时为空都合法（空项入队前被 enrich 的 id0/arg 门拦掉）。
type platformEnrichJob struct {
	id     int64
	domain string
	url    string
}

// enrichPlatformAssets 平台侧资产入库的补全出口（W7，方案 v10 §6/§15）。
//
// enrich 引擎此前只挂在 worker 工具层（ToolSet.SetEnrich），平台侧两个入库口
// ——手工资产 API 与 ScopeSentry 同步——从不触发补全，登记进来的域名/URL 永远
// 没有 DNS/HTTP 留痕。这里统一接上：新域名 → ResolveDomain（解析回写 IP/记录）、
// 新 URL → ProbeSite（探测回写状态码/标题；引擎用自带 client，不进任务出网代理），
// 并留一条 kind=note / worker=system 的 activity（如实标注「平台侧补全」；activity
// 的 FK 挂 exploration，无任务上下文 ⇒ 只补全不留痕）。
//
// best-effort：单独 goroutine + 5s 超时兜底，enrich 未装配/超时/panic 一律只 log，
// 不影响入库响应。句柄取 Manager.Enrich()（manager.go 装配的唯一实例）——不再另挂
// Server 字段，避免同一引擎出现两个可达入口。
func (s *Server) enrichPlatformAssets(taskID int64, jobs []platformEnrichJob) {
	eng := s.m.Enrich()
	if eng == nil || s.m.pg == nil || len(jobs) == 0 {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[enrich] 平台侧补全 panic: %v", r)
			}
		}()
		// 5s 超时兜底：补全是入队即返回的异步作业，慢只可能慢在 activity 留痕的
		// DB 写——超时只放弃等待并留日志，goroutine 随调用自然结束。
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				log.Printf("[enrich] 平台侧补全留痕超时(>5s)，放弃等待")
			}
		}()
		var domains, urls int
		for _, j := range jobs {
			if j.domain != "" {
				eng.ResolveDomain(j.id, j.domain)
				domains++
			}
			if j.url != "" {
				eng.ProbeSite(j.id, j.url)
				urls++
			}
		}
		if taskID <= 0 {
			return // 无任务上下文：activity FK 挂 exploration，留不了痕
		}
		t, err := s.m.pg.GetTask(taskID)
		if err != nil || t == nil || t.ExplorationID <= 0 {
			return
		}
		if _, err := s.m.pg.Exploration(t.ExplorationID).AppendActivity(db.Activity{
			Worker: "system", Kind: "note",
			Summary: fmt.Sprintf("平台侧补全：已触发 DNS 解析 %d 个域名、HTTP 探测 %d 个 URL", domains, urls),
		}); err != nil {
			log.Printf("[enrich] 平台侧补全留痕失败（task %d）: %v", taskID, err)
		}
	}()
}
