package evidence

import (
	"context"
	"os"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/testdb"
)

// 给本包准备一个独立的测试库，并持有集群级 advisory lock 直到测试结束。
//
// 各包共用同一个库时，前一个包留下的数据会让后一个包的用例莫名其妙地挂
// （实测每次挂的用例都不一样）。派生独立库后各包互不可见，套件才可重复。
func TestMain(m *testing.M) {
	dsn, release, err := testdb.Setup("evidence")
	if err != nil {
		os.Exit(m.Run())
	}
	defer release()
	_ = os.Setenv("ARTEX_PG_DSN", dsn)
	os.Exit(runEvidenceSuite(m))
}
func runEvidenceSuite(m *testing.M) int {
	pg, err := db.Open(os.Getenv("ARTEX_PG_DSN"))
	if err != nil {
		panic(err)
	}
	defer pg.Close()
	conn, err := pg.Conn(context.Background())
	if err != nil {
		panic(err)
	}
	defer conn.Close()
	return m.Run()
}
