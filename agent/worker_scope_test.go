package agent

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

// promptCaptureProvider 是一个「立刻收工」的假 provider：它把每次请求的 system
// 提示原文记下来，模型侧只回一句纯文本（不带任何工具调用）⇒ 一次 run 就结束。
//
// 为什么用它而不是断言某个内部函数：worker 真正发给模型的那份 system 提示就是
// 这里拼出来的，断言它才能证明「模型上下文里没有范围外的资产」。
type promptCaptureProvider struct {
	systems []string
}

func (p *promptCaptureProvider) record(req llm.CompletionRequest) {
	p.systems = append(p.systems, strings.Join(req.System, "\n"))
}

func (p *promptCaptureProvider) Stream(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	p.record(req)
	return func(yield func(llm.StreamEvent, error) bool) {
		if !yield(llm.StreamEvent{Type: llm.SEMessageStart}, nil) {
			return
		}
		if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "done"}, nil) {
			return
		}
		if !yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil) {
			return
		}
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}

func (p *promptCaptureProvider) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.record(req)
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
}

// systemPrompt 返回最近一次请求的 system 提示全文。
func (p *promptCaptureProvider) systemPrompt(t *testing.T) string {
	t.Helper()
	if len(p.systems) == 0 {
		t.Fatal("provider 没收到任何请求：worker 没有真正跑起来")
	}
	return p.systems[len(p.systems)-1]
}

// scopePromptFixture 铺出「调用者任务 + 范围内资产 + 范围外资产（带凭据）」这套场景。
//
// 范围外的资产挂在【另一个任务】名下并带着 auth 凭据 —— 正是审计里 L1 描述的形状：
// 一旦 worker 用全局读取意图里的 asset_ids，这条资产（连同凭据）会整行进 system 提示。
type scopePromptFixture struct {
	db        *db.DB
	task      *db.Task
	intent    *db.Node
	inHost    string
	outHost   string
	outSecret string
}

// newScopePromptFixture 建 fixture：调用者任务只声明 inHost 这个根域的范围。
func newScopePromptFixture(t *testing.T) *scopePromptFixture {
	t.Helper()
	d := testDB(t)
	t.Cleanup(func() { _ = d.Close() })

	stamp := time.Now().UnixNano()
	inHost := fmt.Sprintf("worker-scope-in-%d.example.test", stamp)
	outHost := fmt.Sprintf("worker-scope-out-%d.example.test", stamp)
	secret := fmt.Sprintf("S3CRET-worker-scope-%d", stamp)

	task, err := d.CreateTask("worker scope fixture", "读面范围隔离", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	other, err := d.CreateTask("worker scope other task", "别人的任务", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(other.ID) })

	// 调用者任务的测试范围：只声明 inHost 这个根域。
	// 直接写 task_scope 行：agent 自主扩范围的白名单刻意不含 root_domain，
	// fixture 要的是「已声明的范围」，不是「agent 扩出来的范围」。
	seedTaskScope(t, d, task.ID, "root_domain", inHost)

	inID := insertScopeAsset(t, d, scopeAssetSpec{domain: inHost, rootDomain: inHost, ownerTaskID: task.ID})
	outID := insertScopeAsset(t, d, scopeAssetSpec{domain: outHost, rootDomain: outHost, ownerTaskID: other.ID, secret: secret})
	t.Cleanup(func() {
		d.Exec(`DELETE FROM assets WHERE id=ANY($1::bigint[])`, []int64{inID, outID}) //nolint:errcheck
	})

	ts := d.Exploration(task.ExplorationID)
	nodeID, err := ts.AddNode(db.KindIntent, map[string]any{
		"text":      "探测意图锚定的目标资产",
		"asset_ids": []int64{inID, outID},
	}, 0, "open", "planner", nil)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := ts.ListByKind(db.KindIntent, 10)
	if err != nil {
		t.Fatal(err)
	}
	var intent *db.Node
	for _, n := range nodes {
		if n.ID == nodeID {
			intent = n
		}
	}
	if intent == nil {
		t.Fatalf("意图节点 %d 没建出来", nodeID)
	}

	return &scopePromptFixture{
		db:        d,
		task:      task,
		intent:    intent,
		inHost:    inHost,
		outHost:   outHost,
		outSecret: secret,
	}
}

// seedTaskScope 直接写一条 task_scope 行（已声明的范围）。
func seedTaskScope(t *testing.T, d *db.DB, taskID int64, kind, domain string) {
	t.Helper()
	if _, err := d.Exec(`INSERT INTO task_scope(task_id,kind,domain,source) VALUES ($1,$2,$3,'auto')
ON CONFLICT DO NOTHING`, taskID, kind, domain); err != nil {
		t.Fatal(err)
	}
}

// scopeAssetSpec 描述一条要插进 assets 的资产（范围/归属精确可控）。
type scopeAssetSpec struct {
	domain      string
	rootDomain  string
	ownerTaskID int64
	secret      string // 非空时挂一条 auth 凭据
}

// insertScopeAsset 直接插一条资产。
func insertScopeAsset(t *testing.T, d *db.DB, spec scopeAssetSpec) int64 {
	t.Helper()
	var id int64
	err := d.QueryRow(`INSERT INTO assets(type,domain,root_domain,task_ids,auth,last_seen)
VALUES ('subdomain',$1,$2,ARRAY[$3]::bigint[],
        CASE WHEN $4='' THEN '{}'::jsonb[]
             ELSE ARRAY[jsonb_build_object('type','password','username','root','password',$4)]::jsonb[] END,
        now()) RETURNING id`, spec.domain, spec.rootDomain, spec.ownerTaskID, spec.secret).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// runWorkerWithPrompt 跑一次 worker，返回模型实际收到的 system 提示。
func (f *scopePromptFixture) runWorkerWithPrompt(t *testing.T, taskID int64) string {
	t.Helper()
	prov := &promptCaptureProvider{}
	w := NewWorker(prov, "fixture-model", t.TempDir(), nil, 100000, 2)
	if _, _, err := w.Execute(context.Background(), "worker", taskID, f.db.Assets(), f.db.Exploration(f.task.ExplorationID), f.intent, nil, nil, nil, nil); err != nil {
		t.Fatalf("worker run: %v", err)
	}
	return prov.systemPrompt(t)
}

// TestWorkerSystemPromptExcludesOutOfScopeAssets pins L1：worker 的 system 提示里
// 只能出现【本任务范围内】的资产。范围外的资产（连同它的 auth 凭据）一个字都不能
// 进提示 —— 提示是模型的上下文，等于把凭据交给了不该看到它的 agent。
func TestWorkerSystemPromptExcludesOutOfScopeAssets(t *testing.T) {
	fx := newScopePromptFixture(t)

	prompt := fx.runWorkerWithPrompt(t, fx.task.ID)

	if strings.Contains(prompt, fx.outHost) {
		t.Fatalf("worker system 提示里出现了范围外资产 %s（范围隔离失效）", fx.outHost)
	}
	if strings.Contains(prompt, fx.outSecret) {
		t.Fatalf("worker system 提示里出现了范围外资产的凭据（凭据泄露）")
	}
	if !strings.Contains(prompt, fx.inHost) {
		t.Fatalf("worker system 提示里缺少范围内资产 %s（范围内的目标资产必须照常下发）", fx.inHost)
	}
}

// TestWorkerSystemPromptWithoutTaskContextExposesNoAssets pins fail-closed：
// 没有任务上下文（taskID<=0）就没有授权边界，此时范围解析为平台范围（默认空），
// 一个资产都不能给 —— 绝不是「全都给」。
func TestWorkerSystemPromptWithoutTaskContextExposesNoAssets(t *testing.T) {
	fx := newScopePromptFixture(t)

	prompt := fx.runWorkerWithPrompt(t, 0)

	if strings.Contains(prompt, fx.inHost) {
		t.Fatalf("无任务上下文时 worker system 提示里出现了资产 %s（必须是 fail-closed：一个都不给）", fx.inHost)
	}
	if strings.Contains(prompt, fx.outHost) {
		t.Fatalf("无任务上下文时 worker system 提示里出现了资产 %s（必须是 fail-closed：一个都不给）", fx.outHost)
	}
}
