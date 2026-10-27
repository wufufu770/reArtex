package agent

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
// 各包共用同一个库时，前一个包留下的数据会让后一个包的用例莫名其妙地挂
// （实测每次挂的用例都不一样）。派生独立库后各包互不可见，套件才可重复。
func TestMain(m *testing.M) {
	dsn, release, err := testdb.Setup("agent")
	if err != nil {
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
