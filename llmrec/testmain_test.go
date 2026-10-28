package llmrec

import (
	"database/sql"
	"os"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/testdb"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestMain 给本包准备一个独立的测试库。
//
// 本包原先没有取锁，于是它的测试会与其它包**并发**跑在同一个库上：本包的
// db.Open 会触发建表 DDL，而那些包正在做 DML，双方互相阻塞 —— 表现是每次
// 全量跑挂的用例都不一样（单独跑却全过）。现在各包各有各的库，交叉干扰消失。
func TestMain(m *testing.M) {
	dsn, release, err := testdb.Setup("llmrec")
	if err != nil {
		// No DB configured — tests that need PG will skip themselves.
		os.Exit(m.Run())
	}
	_ = os.Setenv("ARTEX_PG_DSN", dsn)

	code := runSuite(m, dsn)
	release()
	os.Exit(code)
}

// runSuite 在派生库上跑用例。os.Exit 不执行 defer，所以收尾（关连接、删库）
// 必须在退出前显式做完。
func runSuite(m *testing.M, dsn string) int {
	conn, err := sql.Open("pgx", dsn)
	if err != nil || conn.Ping() != nil {
		return m.Run()
	}
	defer conn.Close()

	// llm_usage 的建表语句不在 schema.sql 里，而是由应用启动时调用
	// EnsureLLMUsageTable 单独执行（server/manager.go）。派生库是全新的，
	// 没人建这张表，计量用例就会报 relation "llm_usage" does not exist。
	if pg, err := db.Open(dsn); err == nil {
		_ = pg.EnsureLLMUsageTable()
		pg.Close()
	}
	return m.Run()
}
