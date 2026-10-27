package agent

import (
	"database/sql"
	"os"
	"testing"

	"github.com/Autumn-27/artex/internal/testdb"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestMain 给本包准备一个独立的测试库。
//
// 各包共用同一个库时，前一个包留下的数据会让后一个包的用例莫名其妙地挂
// （实测每次挂的用例都不一样）。派生独立库后各包互不可见，套件才可重复。
func TestMain(m *testing.M) {
	dsn, release, err := testdb.Setup("agent")
	if err != nil {
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
	return m.Run()
}
