// Package testdb 给测试套件提供「每包独立库」的连接串。
//
// 为什么派生独立库：各测试包共用同一个库时，前一个包留下的数据会让后一个包的
// 用例莫名其妙地挂 —— 实测基线上 server 包每次挂的用例都不一样（一次 10 个、
// 一次 16 个，测试名都不同）。派生独立库后各包互不可见，套件才可重复，
// CI 门才能一直绿着有约束力。
//
// 绝不碰配置里的那个库：这里只重建「派生名」的库（固定 artex_test_ 前缀），
// 开发/生产库完全不受影响。顺带的好处：测试不再往开发库里灌数据。
package testdb

import (
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	"github.com/Autumn-27/artex/config"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// advisoryLockID 与各测试包 TestMain 里用的锁号一致（集群级互斥）。
//
// 锁在重建库之前取：否则两个并发的同名包运行会互相把对方正在用的库删掉。
const advisoryLockID = 7337741002

// Setup 返回按包名派生的独立测试库连接串，并把该库重建为空库。
//
// 返回的 release 必须被调用（defer），它会释放集群级锁。
//
// pkg 传包名（如 "server"）。返回空串表示没有可用的数据库配置 —— 调用方
// 应当照旧跳过需要库的用例。
//
// 锁在整个测试运行期间持有：重建库与运行用例之间若有间隙，并发的同名包运行
// 会把对方正在用的库删掉。
func Setup(pkg string) (dsn string, release func(), err error) {
	base, _, err := config.PostgresDSN()
	if err != nil {
		return "", func() {}, err
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", func() {}, err
	}
	name := "artex_test_" + sanitize(pkg)

	// 用维护库连接：先取集群级锁，再重建目标库。
	admin := *u
	admin.Path = "/postgres"
	conn, err := sql.Open("pgx", admin.String())
	if err != nil {
		return "", func() {}, err
	}
	if _, err := conn.Exec(fmt.Sprintf("SELECT pg_advisory_lock(%d)", advisoryLockID)); err != nil {
		conn.Close()
		return "", func() {}, err
	}
	unlock := func() {
		conn.Exec(fmt.Sprintf("SELECT pg_advisory_unlock(%d)", advisoryLockID)) //nolint:errcheck
		conn.Close()
	}

	// WITH (FORCE) 踢掉残留连接：上一次运行可能还有连接挂在库上。
	if _, err := conn.Exec(fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, name)); err != nil {
		unlock()
		return "", func() {}, err
	}
	if _, err := conn.Exec(fmt.Sprintf(`CREATE DATABASE %q`, name)); err != nil {
		unlock()
		return "", func() {}, err
	}

	u.Path = "/" + name
	return u.String(), unlock, nil
}

// sanitize 把包名变成合法的库名片段。
func sanitize(pkg string) string {
	var b strings.Builder
	for _, r := range pkg {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
