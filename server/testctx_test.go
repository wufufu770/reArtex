package server

import (
	"context"
	"testing"
)

// testServerCtx 造一个**可取消**的 ctx 供测试里的 Server 用，并在用例结束时取消。
//
// 为什么不能用 context.Background()：server.New 会起若干后台循环
// （notifier 的 Run 每 3 秒一轮、task-archive worker、reconcileConcurrency），
// 它们靠 <-ctx.Done() 退出。传 Background() ⇒ 这些循环**永生**，在同一个包里
// 跨用例存活，继续读写【共享的包级派生库】—— 同包另一个用例的待投递行会被
// 抢去分派，表现为那个用例「没有任何进展」这类查不到原因的失败。
//
// 生产代码不受影响：cmd/artex/main.go 传的是信号 ctx，进程退出时会被取消。
func testServerCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}
