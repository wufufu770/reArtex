package agent

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/traffic"
)

// TestEgressTaskIDPrefersExplicitTask pins that a task-scoped run keeps its own
// task identity even when the context carries a different one.
func TestEgressTaskIDPrefersExplicitTask(t *testing.T) {
	ctx := WithRunInfo(context.Background(), RunInfo{TaskID: 7})
	if got := egressTaskID(ctx, 42); got != 42 {
		t.Fatalf("显式 taskID 优先: got %d, want 42", got)
	}
}

// TestEgressTaskIDFallsBackToRunInfo pins that a chat agent in a group session
// (no explicit taskID parameter) still gets the task identity from the context —
// otherwise group members' egress would be unattributed and escape scope checks.
func TestEgressTaskIDFallsBackToRunInfo(t *testing.T) {
	ctx := WithRunInfo(context.Background(), RunInfo{TaskID: 7})
	if got := egressTaskID(ctx, 0); got != 7 {
		t.Fatalf("应回落到 ctx 里的任务身份: got %d, want 7", got)
	}
}

// TestEgressTaskIDZeroWhenNoTask pins that a standalone chat session stays
// unattributed: 没有任务就没有范围边界，不能伪造身份。
func TestEgressTaskIDZeroWhenNoTask(t *testing.T) {
	ctx := WithRunInfo(context.Background(), RunInfo{SessionID: "conv-3"})
	if got := egressTaskID(ctx, 0); got != 0 {
		t.Fatalf("没有任务时必须返回 0: got %d", got)
	}
}

// TestEgressTaskIDMergedRunInfo pins that the merge path (WithRunInfoMerged)
// keeps TaskID — the chat agent merges a session id into an existing RunInfo,
// and dropping TaskID there would silently un-scope every group member.
func TestEgressTaskIDMergedRunInfo(t *testing.T) {
	ctx := WithRunInfo(context.Background(), RunInfo{TaskID: 7, ExplorationID: 9})
	ctx = WithRunInfoMerged(ctx, RunInfo{SessionID: "conv-3"})
	if got := egressTaskID(ctx, 0); got != 7 {
		t.Fatalf("合并 RunInfo 后必须保住 TaskID: got %d, want 7", got)
	}
}

// TestEgressProxyURLCarriesTaskIdentity pins that a proxy URL used by an
// outbound channel carries the task identity: 代理靠 Proxy-Authorization 里的
// 令牌识别归属，没有令牌 ⇒ 请求被判「未归属」⇒ 不受任务范围校验。
func TestEgressProxyURLCarriesTaskIdentity(t *testing.T) {
	raw := egressProxyURL("http://127.0.0.1:8788", 42)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("代理地址解析失败: %v", err)
	}
	if u.User == nil {
		t.Fatalf("代理地址必须带身份令牌: %s", raw)
	}
	token, _ := u.User.Password()
	if token == "" {
		token = u.User.Username()
	}
	if token != traffic.SignEgressTaskToken(42) {
		t.Fatalf("身份令牌不对: %q", token)
	}
}

// TestEgressProxyURLNoTaskNoIdentity pins that no task means no identity:
// 没有任务就没有范围边界，伪造身份比不带更危险。
func TestEgressProxyURLNoTaskNoIdentity(t *testing.T) {
	raw := egressProxyURL("http://127.0.0.1:8788", 0)
	if raw != "http://127.0.0.1:8788" {
		t.Fatalf("没有任务时不该改写代理地址: %s", raw)
	}
}

// TestEgressProxyURLKeepsSocks5Intact pins that socks5 proxies are left alone:
// socks5 的 userinfo 是另一套认证，塞进去会把代理弄坏。
func TestEgressProxyURLKeepsSocks5Intact(t *testing.T) {
	raw := egressProxyURL("socks5://10.0.0.1:1080", 42)
	if raw != "socks5://10.0.0.1:1080" {
		t.Fatalf("socks5 代理地址被改动了: %s", raw)
	}
}

// TestWebFetchProxyAlwaysCarriesIdentity is a guard against a new agent role
// (or a refactor) quietly reintroducing an unattributed outbound channel:
// every WebFetchProxy assignment in the agent package must go through
// egressProxyURL so the request carries the task identity.
func TestWebFetchProxyAlwaysCarriesIdentity(t *testing.T) {
	files := []string{"worker.go", "planner.go", "mainagent.go", "chat.go"}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", f, err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "WebFetchProxy:") {
				continue
			}
			if !strings.Contains(trimmed, "egressProxyURL(") {
				t.Errorf("%s:%d 的 WebFetchProxy 没带任务身份（必须经 egressProxyURL）: %s",
					f, i+1, trimmed)
			}
		}
	}
}
