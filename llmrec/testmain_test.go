package llmrec

import (
	"database/sql"
	"os"
	"testing"

	"github.com/Autumn-27/artex/db"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestMain acquires a PostgreSQL advisory lock (7337741002) for the entire
// llmrec test suite.
//
// 本包原先没有取锁，于是它的测试会与 agent / db / server / evidence 四个包
// **并发**跑在同一个库上：本包的 db.Open 会触发建表 DDL，而那些包正在做 DML，
// 双方互相阻塞 —— 表现是每次全量跑挂的用例都不一样（agent 包里的黑板用例、
// 本包的 side usage 用例随机中招），单独跑却全过。
//
// 取同一把锁后各包串行，交叉干扰消失。锁号与 db/agent/server/evidence 一致。
func TestMain(m *testing.M) {
	dsn, _, err := db.DSN()
	if err != nil {
		// No DB configured — tests that need PG will skip themselves.
		os.Exit(m.Run())
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil || conn.Ping() != nil {
		os.Exit(m.Run())
	}
	defer conn.Close()
	if _, err := conn.Exec(`SELECT pg_advisory_lock(7337741002)`); err != nil {
		os.Exit(m.Run())
	}
	defer conn.Exec(`SELECT pg_advisory_unlock(7337741002)`) //nolint:errcheck
	os.Exit(m.Run())
}
