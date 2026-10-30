package llmrec

import (
	"context"
	"database/sql"
	"errors"
	"iter"
	"os"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
	"github.com/google/uuid"
)

// usageEventProvider 是一个假的 inner provider：按给定顺序产出事件。
//
// 用它而不是直接调 llm.Usage.MergeEcho，是因为后者对本次改动**不敏感** ——
// 改 llmrec.go 前后都会绿。真正要锁的是 Recorder 的折叠路径本身。
type usageEventProvider struct {
	completeProvider
	events []llm.StreamEvent
}

func (p usageEventProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		for _, ev := range p.events {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

// recordStreamUsage 驱动 Recorder 的事件循环，把落库的计量行读回来。
//
// 读回的是**折叠之后**的用量，即账本真正记下来的数。
func recordStreamUsage(t *testing.T, events []llm.StreamEvent) (input, output, cacheRead, cacheWrite int) {
	t.Helper()
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("requires isolated ARTEX_PG_DSN")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()

	profile := "usage-fold-" + uuid.NewString()
	ctx := transcript.WithSessionID(t.Context(), "exp0-usage-fold")
	recorder := Wrap(usageEventProvider{events: events}, pg, "fixture", profile, "", "", func() bool { return false })
	for _, streamErr := range recorder.Stream(ctx, llm.CompletionRequest{}) {
		if streamErr != nil {
			t.Fatal(streamErr)
		}
	}

	// 计量行是流结束时写的；机器一忙可能还没到，故轮询等它出现而不是查一次就下结论。
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = pg.QueryRow(`SELECT input_tokens, output_tokens, cache_read, cache_write
FROM llm_usage WHERE profile_name=$1`, profile).Scan(&input, &output, &cacheRead, &cacheWrite)
		if err == nil {
			break
		}
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("计量行未出现")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, _ = pg.Exec(`DELETE FROM llm_usage WHERE profile_name=$1`, profile)
	return input, output, cacheRead, cacheWrite
}

// 真的遵循 Anthropic 约定的 provider：输入侧在 message_start 与 message_delta
// **各报一次**（回显），输出侧只在 delta 报。
//
// 这里的用量是 SDK 规范化之后的形态（norma/llm/anthropic.go 的 anthropicUsage.norm
// 把 cache 折进 InputTokens；message_start 的 output 占位符被 SDK 抹成 0）。
//
// 求和会把输入侧翻倍（740+740=1480）；取大则保持 740。
func TestStreamFoldsFullyEchoedUsageWithoutDoubleCounting(t *testing.T) {
	input, output, cacheRead, cacheWrite := recordStreamUsage(t, []llm.StreamEvent{
		{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 740, CacheReadTokens: 200, CacheWriteTokens: 40}},
		{Type: llm.SEMessageDelta, Usage: llm.Usage{InputTokens: 740, CacheReadTokens: 200, CacheWriteTokens: 40, OutputTokens: 77}},
	})
	if input != 740 {
		t.Errorf("input = %d, want 740 (回显的输入侧不能求和)", input)
	}
	if cacheRead != 200 {
		t.Errorf("cache_read = %d, want 200", cacheRead)
	}
	if cacheWrite != 40 {
		t.Errorf("cache_write = %d, want 40", cacheWrite)
	}
	if output != 77 {
		t.Errorf("output = %d, want 77", output)
	}
}

// Anthropic 兼容的模拟端点（实测 MiniMax 就是这个形状）：message_start 带全 0，
// 全部用量只在 message_delta。
//
// 用量必须被**恢复**出来，而不是因为 start 是 0 就记成 0。
func TestStreamRecoversUsageReportedOnlyInDelta(t *testing.T) {
	input, output, cacheRead, cacheWrite := recordStreamUsage(t, []llm.StreamEvent{
		{Type: llm.SEMessageStart, Usage: llm.Usage{}},
		{Type: llm.SEMessageDelta, Usage: llm.Usage{InputTokens: 207, CacheReadTokens: 197, OutputTokens: 39}},
	})
	if input != 207 {
		t.Errorf("input = %d, want 207 (delta 报的真实输入侧必须被恢复)", input)
	}
	if cacheRead != 197 {
		t.Errorf("cache_read = %d, want 197", cacheRead)
	}
	if output != 39 {
		t.Errorf("output = %d, want 39", output)
	}
	if cacheWrite != 0 {
		t.Errorf("cache_write = %d, want 0", cacheWrite)
	}
}

// 部分回显：delta 报了比 start 更大的输入侧，却**没带**缓存字段。
//
// 取大则输入侧取 500、缓存从 start 存活；求和会得到 600（并且缓存字段照样保留，
// 掩盖掉真正的错误）。这条锁的是「逐字段取大」，不是「整包取一」。
func TestStreamPartialEchoTakesPerFieldMaximum(t *testing.T) {
	input, output, cacheRead, cacheWrite := recordStreamUsage(t, []llm.StreamEvent{
		{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 100, CacheReadTokens: 10, CacheWriteTokens: 5}},
		{Type: llm.SEMessageDelta, Usage: llm.Usage{InputTokens: 500, OutputTokens: 20}},
	})
	if input != 500 {
		t.Errorf("input = %d, want 500 (逐字段取大，不是求和 600)", input)
	}
	if cacheRead != 10 {
		t.Errorf("cache_read = %d, want 10 (delta 未带缓存，必须从 start 存活)", cacheRead)
	}
	if cacheWrite != 5 {
		t.Errorf("cache_write = %d, want 5 (delta 未带缓存，必须从 start 存活)", cacheWrite)
	}
	if output != 20 {
		t.Errorf("output = %d, want 20", output)
	}
}
