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

// advisoryLockID 是集群级互斥锁：重建库这一步必须互斥，否则两个并发的同名
// 包运行会互相把对方正在用的库删掉。
const advisoryLockID = 7337741002

// Setup 返回按包名派生的独立测试库连接串，并把该库重建为空库。
//
// 返回的 release 必须被调用，它会删掉派生库。pkg 传包名（如 "server"）。
// 返回空串表示没有可用的数据库配置 —— 调用方应当照旧跳过需要库的用例。
//
// 锁只在重建库期间持有，跑用例时不持有：各包各用各的库，本来就不需要互斥，
// 而把锁持到测试跑完会让整个套件退化成串行（全量实测 llmrec 包从 10 秒
// 变成 304 秒，全靠排队等前面的包）。
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

	admin := *u
	admin.Path = "/postgres"
	conn, err := sql.Open("pgx", admin.String())
	if err != nil {
		return "", func() {}, err
	}
	if err := rebuild(conn, name); err != nil {
		conn.Close()
		return "", func() {}, err
	}

	u.Path = "/" + name
	return u.String(), func() {
		drop(conn, name) //nolint:errcheck
		conn.Close()
	}, nil
}

// rebuild 在集群锁下把目标库重建为空库。
func rebuild(conn *sql.DB, name string) error {
	if err := drop(conn, name); err != nil {
		return err
	}
	if _, err := conn.Exec(fmt.Sprintf(`CREATE DATABASE %q`, name)); err != nil {
		return err
	}
	return nil
}

// drop 在集群锁下删掉目标库。
//
// 删库和建库必须分两步：收尾时只要「删掉」，如果顺手又建一个空库，测完
// 会在 postgres 里留下一堆 artex_test_* 空壳。
func drop(conn *sql.DB, name string) error {
	if _, err := conn.Exec(fmt.Sprintf("SELECT pg_advisory_lock(%d)", advisoryLockID)); err != nil {
		return err
	}
	defer conn.Exec(fmt.Sprintf("SELECT pg_advisory_unlock(%d)", advisoryLockID)) //nolint:errcheck

	// WITH (FORCE) 踢掉残留连接：上一次运行可能还有连接挂在库上。
	if _, err := conn.Exec(fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, name)); err != nil {
		return err
	}
	return nil
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
