package traffic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

// scopeTaskKey 是本测试用来标记「本次调用属于哪个任务」的 ctx key。
//
// 真实实现里这个角色由 agent.RunInfoFrom(ctx) 承担；流量包不能 import agent
// （agent → evidence → traffic 会成导入环），所以范围判定被抽象成 ScopeGate
// 接口，由调用方注入 —— 这里注入的是测试替身。
type scopeTaskKey struct{}

// scopeGateStub 是范围门的测试替身：按 ctx 里的任务号给出可读 host 集合；
// 没有任务上下文时返回错误（与真实实现的语义一致）。
type scopeGateStub struct {
	hosts map[int64][]string
}

// AllowedHosts implements ScopeGate.
func (g scopeGateStub) AllowedHosts(ctx context.Context) ([]string, error) {
	id, _ := ctx.Value(scopeTaskKey{}).(int64)
	if id <= 0 {
		return nil, fmt.Errorf("缺少任务上下文：读取流量必须绑定到具体任务")
	}
	return g.hosts[id], nil
}

// scopeCtx 造一个带任务号的 ctx。
func scopeCtx(taskID int64) context.Context {
	return context.WithValue(context.Background(), scopeTaskKey{}, taskID)
}

// trafficTool 取出流量包暴露的某个工具。
func trafficTool(t *testing.T, tr *Traffic, name string) actool.CoreTool {
	t.Helper()
	for _, tool := range tr.Tools() {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("缺少工具：%s", name)
	return nil
}

// callTrafficTool 调用一个流量工具，返回展开后的文本与是否为错误。
func callTrafficTool(t *testing.T, tr *Traffic, ctx context.Context, name string, input any) (string, bool) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	result, err := trafficTool(t, tr, name).Call(ctx, raw, nil)
	if err != nil {
		t.Fatalf("%s 调用失败：%v", name, err)
	}
	return result.Flatten(), result.IsError
}

// seedScopeTraffic 造两个 host 的流量：一个属于任务 A，一个属于任务 B。
// 返回 (A 的流量 id, B 的流量 id, A 的 blob hash, B 的 blob hash)。
func seedScopeTraffic(t *testing.T, tr *Traffic, dir string) (string, string, string, string) {
	t.Helper()
	write := func(id, host, body string) {
		if _, err := tr.DB().Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES(?,1,?,'GET','/','http://'||?||'/',200,'text/plain',?,?,'')`,
			id, host, host, len(body), len(body)); err != nil {
			t.Fatal(err)
		}
		if _, err := tr.DB().Exec(`INSERT INTO exchange_bodies(id,req_head,resp_head,req_body,resp_body)
VALUES(?,'GET / HTTP/1.1','HTTP/1.1 200 OK','',?)`, id, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	write("a-1", "a.example.com", "task A secret body")
	write("b-1", "b.example.com", "task B credential body")

	blob := func(id, body string) string {
		sum := sha256.Sum256([]byte(body))
		hash := hex.EncodeToString(sum[:])
		path := filepath.Join(dir, "_blobs", "sha256", hash[:2], hash+".bin")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := tr.DB().Exec(`INSERT OR IGNORE INTO blob_refs(hash,exchange_id) VALUES(?,?)`, hash, id); err != nil {
			t.Fatal(err)
		}
		return hash
	}
	return "a-1", "b-1", blob("a-1", "task A spilled body"), blob("b-1", "task B spilled body")
}

// TestTrafficSearchRefusesHostOutsideCallerScope pins the L2 core guarantee:
// traffic captured for a host that is not inside the caller's scope must not be
// readable — not through traffic_search, not through traffic_get, and not
// through traffic_blob.
func TestTrafficSearchRefusesHostOutsideCallerScope(t *testing.T) {
	dir := t.TempDir()
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	tr.SetScopeGate(scopeGateStub{hosts: map[int64][]string{1: {"a.example.com"}}})
	aID, bID, aBlob, bBlob := seedScopeTraffic(t, tr, dir)

	ctx := scopeCtx(1)
	out, isErr := callTrafficTool(t, tr, ctx, "traffic_search", map[string]any{"host": "b.example.com"})
	if !isErr {
		t.Fatalf("范围外 host 的流量必须被拒绝，实际返回：%s", out)
	}
	if !strings.Contains(out, "范围内") {
		t.Fatalf("拒绝文案必须说明原因（不在范围内），实际：%s", out)
	}
	if strings.Contains(out, "task B credential body") {
		t.Fatalf("拒绝文案泄露了范围外内容：%s", out)
	}

	if out, isErr = callTrafficTool(t, tr, ctx, "traffic_get", map[string]any{"id": bID}); !isErr {
		t.Fatalf("范围外流量的原文必须被拒绝，实际返回：%s", out)
	}
	if out, isErr = callTrafficTool(t, tr, ctx, "traffic_blob", map[string]any{"hash": bBlob}); !isErr {
		t.Fatalf("范围外流量的分段正文必须被拒绝，实际返回：%s", out)
	}
	if out, isErr = callTrafficTool(t, tr, ctx, "traffic_get", map[string]any{"id": aID}); isErr {
		t.Fatalf("范围内的流量必须可读：%s", out)
	}
	if out, isErr = callTrafficTool(t, tr, ctx, "traffic_blob", map[string]any{"hash": aBlob}); isErr {
		t.Fatalf("范围内的分段正文必须可读：%s", out)
	}
}

// TestTrafficReadsRequireTaskContext pins the fail-closed half of L2: a caller
// without a task context has no authorization edge at all, so it must be
// refused rather than served from the global traffic pool.
func TestTrafficReadsRequireTaskContext(t *testing.T) {
	dir := t.TempDir()
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	tr.SetScopeGate(scopeGateStub{hosts: map[int64][]string{}})
	aID, _, aBlob, _ := seedScopeTraffic(t, tr, dir)

	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		input any
	}{
		{"traffic_search", map[string]any{"host": "a.example.com"}},
		{"traffic_get", map[string]any{"id": aID}},
		{"traffic_blob", map[string]any{"hash": aBlob}},
	} {
		out, isErr := callTrafficTool(t, tr, ctx, tc.name, tc.input)
		if !isErr {
			t.Fatalf("%s: 没有任务上下文必须被拒绝，实际返回：%s", tc.name, out)
		}
		if !strings.Contains(out, "任务上下文") {
			t.Fatalf("%s: 拒绝文案必须说明缺少任务上下文，实际：%s", tc.name, out)
		}
	}
}

// TestTrafficReadsRefuseWhenScopeGateMissing pins that a traffic store whose
// scope gate was never wired refuses every read instead of falling back to the
// global pool. "范围校验不可用" 必须按"拒绝"处理，不能按"放行"处理。
func TestTrafficReadsRefuseWhenScopeGateMissing(t *testing.T) {
	dir := t.TempDir()
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	aID, _, _, _ := seedScopeTraffic(t, tr, dir)

	out, isErr := callTrafficTool(t, tr, scopeCtx(1), "traffic_get", map[string]any{"id": aID})
	if !isErr {
		t.Fatalf("未配置范围门时必须拒绝读取，实际返回：%s", out)
	}
}
