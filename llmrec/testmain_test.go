package llmrec

import (
	"database/sql"
	"os"
	"testing"

	"github.com/Autumn-27/artex/internal/testdb"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestMain 给本包准备一个独立的测试库，并持有集群级 advisory lock
// (7337741002) 直到测试结束。
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
	defer release()
	_ = os.Setenv("ARTEX_PG_DSN", dsn)

	conn, err := sql.Open("pgx", dsn)
	if err != nil || conn.Ping() != nil {
		os.Exit(m.Run())
	}
	defer conn.Close()
	os.Exit(m.Run())
}
