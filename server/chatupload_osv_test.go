package server

// chatupload_osv_test.go：B-08E 集成验收（live PG + httptest 假 OSV/假 CVE
// MCP，零真实出网）。逐项覆盖卡验收：源码就绪触发一次、无 package.json、
// OSV unavailable 不杀任务、重复触发去重、文件变更手动重跑、OSV 事件写入
// 可见、不阻塞（上传响应即返 + ctx 取消即刻退出）、Unlocked 绝不出网。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/cveintel"
	"github.com/Autumn-27/artex/db"
)

// depAuditFakeOSV 假 OSV querybatch：ok 模式回 lodash@4.17.20 的 GHSA；500
// 模式恒上游错误；slow 模式每请求睡 3s（取消/不阻塞验证用）。
type depAuditFakeOSV struct {
	srv      *httptest.Server
	requests atomic.Int32
	mode     string // ok | 500 | slow
}

func newDepAuditFakeOSV(t *testing.T, mode string) *depAuditFakeOSV {
	t.Helper()
	f := &depAuditFakeOSV{mode: mode}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/querybatch", func(w http.ResponseWriter, r *http.Request) {
		if f.mode == "slow" {
			time.Sleep(3 * time.Second)
		}
		f.requests.Add(1)
		if f.mode == "500" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"vulns":[{"id":"GHSA-35jh-r3h4-6jhm",` +
			`"aliases":["CVE-2020-8203"],` +
			`"affected":[{"package":{"ecosystem":"npm","name":"lodash"},` +
			`"ranges":[{"type":"SEMVER","events":[{"introduced":"0"},{"fixed":"4.17.21"}]}]}]}]}]}`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// depAuditFakeMCP 假 CVE MCP（cve_match_component 信封）：4.17.20→affected，
// 其余→maybe。
func depAuditFakeMCP(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Params struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.Unmarshal(body, &rpc); err != nil {
			t.Errorf("假 MCP 收到非法请求: %v", err)
		}
		ver, _ := rpc.Params.Arguments["version"].(string)
		verdict, reason := "maybe", "fixture 缺省 maybe"
		if ver == "4.17.20" {
			verdict, reason = "affected", "fixture: 4.17.20 < fixed 4.17.21"
		}
		out, _ := json.Marshal([]map[string]string{{"verdict": verdict, "reason": reason}})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"content":[{"type":"text","text":` + strconv.Quote(string(out)) + `}],"is_error":false}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// depAuditSetup 组装 live PG + 假上游 + runner + Server（task 已建、workspace
// 就绪）。返回 task 数值 id 字符串与 workspace 目录。
func depAuditSetup(t *testing.T, osvMode string) (*db.DB, *agent.DepAuditRunner, *Server, string, string, *depAuditFakeOSV) {
	t.Helper()
	pg := cveTestPG(t)
	tk, err := pg.CreateTask("B-08E 集成", "目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(tk.ID) })
	idStr := fmt.Sprint(tk.ID)
	dataDir := t.TempDir()
	ws := filepath.Join(dataDir, "tasks", idStr)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeOSV := newDepAuditFakeOSV(t, osvMode)
	fakeMCP := depAuditFakeMCP(t)
	intel, err := cveintel.New(fakeMCP.URL)
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.NewDepAuditRunner(pg, intel, fakeOSV.srv.URL)
	agent.SetDefaultDepAuditRunner(runner)
	t.Cleanup(func() { agent.SetDefaultDepAuditRunner(nil) })
	m := &Manager{dir: dataDir, pg: pg, tasks: map[string]*Task{idStr: {ID: idStr}}}
	s := &Server{m: m, engine: NewEngine(m)}
	return pg, runner, s, idStr, ws, fakeOSV
}

func depAuditUpload(t *testing.T, s *Server, idStr, name, content string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write([]byte(content))
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/chat/upload?scope=task&id="+idStr, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	s.chatUpload(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("上传应 200: %d %s", rec.Code, rec.Body.String())
	}
}

// depAuditWaitReport 轮询 runner.LastReport（异步完成观测面；条件等待不睡眠）。
func depAuditWaitReport(t *testing.T, runner *agent.DepAuditRunner, taskID int64) *agent.DepAuditReport {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if rep := runner.LastReport(taskID); rep != nil {
			return rep
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("15s 内未见审计报告")
	return nil
}

func depAuditWaitCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("15s 内未满足: %s", what)
}

// depAuditEventCount 数该任务已落的 osv 事件（metadata.dedup_key 前缀=taskID|）。
func depAuditEventCount(t *testing.T, pg *db.DB, taskID int64) int {
	t.Helper()
	var n int
	if err := pg.QueryRow(
		`SELECT COUNT(*) FROM activity WHERE kind='note' AND metadata->>'schema'='osv_event.v1' AND metadata->>'dedup_key' LIKE $1`,
		fmt.Sprintf("%d|%%", taskID)).Scan(&n); err != nil {
		t.Fatalf("事件计数: %v", err)
	}
	return n
}

// depAuditWarningCount 数该任务时间线上的审计 warning note。
func depAuditWarningCount(t *testing.T, pg *db.DB, taskID int64) int {
	t.Helper()
	tk, err := pg.GetTask(taskID)
	if err != nil || tk == nil {
		t.Fatalf("任务查询: %v", err)
	}
	var n int
	if err := pg.QueryRow(
		`SELECT COUNT(*) FROM activity WHERE exploration_id=$1 AND kind='note' AND summary LIKE '%OSV 依赖审计%'`,
		tk.ExplorationID).Scan(&n); err != nil {
		t.Fatalf("warning 计数: %v", err)
	}
	return n
}

func depAuditWriteWorkspace(t *testing.T, ws, manifest, lock string) {
	t.Helper()
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if lock != "" {
		if err := os.WriteFile(filepath.Join(ws, "package-lock.json"), []byte(lock), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const (
	depAuditManifest = `{"name":"ws","dependencies":{"lodash":"^4.17.20"}}`
	depAuditLock     = `{"lockfileVersion":3,"packages":{"node_modules/lodash":{"version":"4.17.20"}}}`
)

// 源码就绪触发一次 + 事件写入可见 + 重复触发去重。
func TestChatUploadDepAuditSourceReady(t *testing.T) {
	pg, runner, s, idStr, ws, _ := depAuditSetup(t, "ok")
	depAuditWriteWorkspace(t, ws, depAuditManifest, depAuditLock)
	taskID, _ := strconv.ParseInt(idStr, 10, 64)

	// 上传（源码就绪时刻）→ 200 → 异步审计完成。
	// 上传件本身落在 uploads/package.json（工作区内）→ 发现面=根清单（锁定）
	// + uploads 副本（无锁→Unlocked 跳过不出网）：Depos=2/Manifests=2/
	// SkippedUnlocked=1 是真实生产形态（只查 Locked）。
	depAuditUpload(t, s, idStr, "package.json", depAuditManifest)
	rep := depAuditWaitReport(t, runner, taskID)
	if rep.Events != 1 || rep.Vulns != 1 || rep.Deps != 2 || rep.Manifests != 2 || rep.SkippedUnlocked != 1 {
		t.Fatalf("首跑报告: %+v", rep)
	}
	// 事件写入可见（时间线读取）。
	if got := depAuditEventCount(t, pg, taskID); got != 1 {
		t.Fatalf("时间线应见恰 1 条 osv 事件: %d", got)
	}
	// 事件字段：fixed_versions/verdict/alias 保留。
	tk, _ := pg.GetTask(taskID)
	page, _, err := pg.Exploration(tk.ExplorationID).ActivityPage(db.ActivitySessionFilter{}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	seenFixed := false
	for _, a := range page {
		if a.Kind != "note" || !strings.Contains(string(a.Metadata), "osv_event.v1") {
			continue
		}
		if strings.Contains(string(a.Metadata), "4.17.21") && strings.Contains(string(a.Metadata), "CVE-2020-8203") {
			seenFixed = true
		}
	}
	if !seenFixed {
		t.Fatal("事件应含 fixed 4.17.21 与 CVE alias")
	}

	// 重复触发（同清单摘要）→ 去重跳过，事件数不变。
	depAuditUpload(t, s, idStr, "package.json", depAuditManifest)
	depAuditWaitCond(t, "重复触发摘要去重", func() bool {
		rep2 := runner.LastReport(taskID)
		return rep2 != nil && rep2.SkippedDigest
	})
	rep2 := runner.LastReport(taskID)
	if rep2 == nil || !rep2.SkippedDigest {
		t.Fatalf("重复触发应摘要去重: %+v", rep2)
	}
	if got := depAuditEventCount(t, pg, taskID); got != 1 {
		t.Fatalf("去重后事件数不变: %d", got)
	}
}

// 无 package.json = 合法空结果（零事件零警告，上传不报错）。
func TestChatUploadDepAuditNoManifest(t *testing.T) {
	pg, runner, s, idStr, ws, fakeOSV := depAuditSetup(t, "ok")
	taskID, _ := strconv.ParseInt(idStr, 10, 64)
	_ = ws
	depAuditUpload(t, s, idStr, "README.md", "纯文档，无清单")
	rep := depAuditWaitReport(t, runner, taskID)
	if !rep.Empty || rep.Deps != 0 || rep.Events != 0 {
		t.Fatalf("无清单应合法空: %+v", rep)
	}
	if got := depAuditEventCount(t, pg, taskID); got != 0 {
		t.Fatalf("无清单零事件: %d", got)
	}
	if fakeOSV.requests.Load() != 0 {
		t.Fatalf("无清单零 OSV 请求: %d", fakeOSV.requests.Load())
	}
	if got := depAuditWarningCount(t, pg, taskID); got != 0 {
		t.Fatalf("合法空不是失败，零警告: %d", got)
	}
}

// OSV unavailable：上传仍 200（不阻塞）、warning 落时间线、零事件——不杀任务。
func TestChatUploadDepAuditOSVUnavailable(t *testing.T) {
	pg, _, s, idStr, ws, _ := depAuditSetup(t, "500")
	depAuditWriteWorkspace(t, ws, depAuditManifest, depAuditLock)
	taskID, _ := strconv.ParseInt(idStr, 10, 64)

	depAuditUpload(t, s, idStr, "package.json", depAuditManifest) // 即返=不阻塞
	depAuditWaitCond(t, "warning 落时间线", func() bool {
		return depAuditWarningCount(t, pg, taskID) > 0
	})
	if got := depAuditEventCount(t, pg, taskID); got != 0 {
		t.Fatalf("上游失败零事件: %d", got)
	}
}

// 文件变更手动重跑（RerunTaskAudit 受控入口）：force 重跑 + B-08D 台账幂等 +
// 变更后再跑 + ctx 取消即刻退出。
func TestDepAuditManualRerunAndChange(t *testing.T) {
	pg, runner, _, idStr, ws, _ := depAuditSetup(t, "ok")
	depAuditWriteWorkspace(t, ws, depAuditManifest, depAuditLock)
	taskID, _ := strconv.ParseInt(idStr, 10, 64)

	// 首跑（手动重跑面 force）。
	rep, err := runner.RerunTaskAudit(context.Background(), taskID, ws)
	if err != nil || rep.Events != 1 {
		t.Fatalf("手动首跑: %+v err=%v", rep, err)
	}
	// 原样重跑：内容未变仍重查（force 跳过摘要闸），但 B-08D 台账幂等
	// → 落的是 deduped 不是重复事件。
	rep2, err := runner.RerunTaskAudit(context.Background(), taskID, ws)
	if err != nil || rep2.DedupedEvents != 1 || rep2.Events != 0 {
		t.Fatalf("重跑应台账幂等: %+v err=%v", rep2, err)
	}
	if got := depAuditEventCount(t, pg, taskID); got != 1 {
		t.Fatalf("重跑后仍恰 1 条: %d", got)
	}
	// 文件变更：加 express 依赖 → 重跑（假 OSV 对 express 回空结果）。
	depAuditWriteWorkspace(t, ws, `{"name":"ws","dependencies":{"lodash":"^4.17.20","express":"^4.18.0"}}`, depAuditLock)
	rep3, err := runner.RerunTaskAudit(context.Background(), taskID, ws)
	if err != nil || rep3.Deps != 2 || rep3.SkippedDigest {
		t.Fatalf("变更后重跑: %+v err=%v", rep3, err)
	}
	// ctx 取消即刻退出（慢上游 + 取消）。
	fakeSlow := newDepAuditFakeOSV(t, "slow")
	intelSlow, err := cveintel.New(depAuditFakeMCP(t).URL)
	if err != nil {
		t.Fatal(err)
	}
	runnerSlow := agent.NewDepAuditRunner(pg, intelSlow, fakeSlow.srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	if _, err := runnerSlow.RerunTaskAudit(ctx, taskID, ws); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消应穿透 context.Canceled: %v", err)
	}
}

// Unlocked 绝不出网：无锁范围串不进 OSV 查询（SkippedUnlocked 如实计数）。
func TestDepAuditUnlockedNeverQueried(t *testing.T) {
	_, runner, _, idStr, ws, fakeOSV := depAuditSetup(t, "ok")
	taskID, _ := strconv.ParseInt(idStr, 10, 64)
	depAuditWriteWorkspace(t, ws, depAuditManifest, "") // 无锁文件
	rep, err := runner.RerunTaskAudit(context.Background(), taskID, ws)
	if err != nil {
		t.Fatal(err)
	}
	if rep.SkippedUnlocked != 1 || !rep.Empty || rep.Vulns != 0 {
		t.Fatalf("Unlocked 应跳过且合法空: %+v", rep)
	}
	if fakeOSV.requests.Load() != 0 {
		t.Fatalf("范围串绝不出网: %d", fakeOSV.requests.Load())
	}
}
