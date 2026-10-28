package guard

import (
	"os"
	"testing"

	"github.com/Autumn-27/artex/internal/testdb"
)

// TestMain 给本包准备一个独立的测试库。
//
// 各包共用同一个库时，前一个包留下的数据会让后一个包的用例莫名其妙地挂
// （实测每次挂的用例都不一样）。派生独立库后各包互不可见，套件才可重复，
// CI 门才能一直绿着有约束力。绝不碰配置里的那个库。
func TestMain(m *testing.M) {
	dsn, release, err := testdb.Setup("guard")
	if err != nil {
		// 无数据库配置：需要库的用例自行跳过。
		os.Exit(m.Run())
	}
	_ = os.Setenv("ARTEX_PG_DSN", dsn)

	// os.Exit 不执行 defer，收尾（删派生库）必须显式做完。
	code := m.Run()
	release()
	os.Exit(code)
}
