package db

// A-12（0.15.78）guard DSL OR 括号化测试：
//  1. 确定性用例——顶层 OR 与 type/task/scope 约束拼接后，OR 分支不得脱离约束；
//  2. 属性对照测试——同一 DSL 表达式，SQL 求值（经 CountDSL，覆盖 AND type=/
//     AND task_ids 两种真实拼接路径）与内存按 AST 求值的集合必须一致。
//
// 资产 DSL 是仓库里唯一的 AND/OR DSL（grep 核实，0.15.78）；括号化属性
// (buildDSLWhereBase 顶层包括号 + build and/or 分支再包一层) 由本文件锁定。

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"time"
)

type bracketAsset struct {
	id      int64
	domain  string
	port    int
	typ     string
	inScope bool
}

// seedBracketAssets 落 16 行夹具：两个 domain 前缀 × 两种 port × 类型/范围交叉。
// 每行 domain 全局唯一（uq_av2_subdomain 对 subdomain 有 (domain,record_type)
// 唯一索引），前缀保留 alpha/beta 供 ILIKE 与内存求值共享同一匹配语义。
func seedBracketAssets(t *testing.T, d *DB, taskID int64) []bracketAsset {
	t.Helper()
	var ids []int64
	out := []bracketAsset{}
	seen := time.Now()
	for _, dom := range []string{"alpha", "beta"} {
		for _, port := range []int{80, 8080} {
			for _, typ := range []string{"subdomain", "service"} {
				for _, inScope := range []bool{true, false} {
					var id int64
					taskIDs := "ARRAY[]::bigint[]"
					if inScope {
						taskIDs = fmt.Sprintf("ARRAY[%d]::bigint[]", taskID)
					}
					fq := fmt.Sprintf("%s-%d-%s-%v.test", dom, port, typ, inScope)
					if err := d.QueryRow(`INSERT INTO assets(type,domain,port,task_ids,last_seen)
						VALUES ($1,$2,$3,`+taskIDs+`,$4) RETURNING id`,
						typ, fq, port, seen).Scan(&id); err != nil {
						t.Fatal(err)
					}
					ids = append(ids, id)
					out = append(out, bracketAsset{id: id, domain: fq, port: port, typ: typ, inScope: inScope})
				}
			}
		}
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM assets WHERE id=ANY($1::bigint[])`, ids) })
	return out
}

// evalBracketNode 在内存里按 AST 对夹具行求值（只支持测试生成器产出的两种叶子：
// domain 的模糊等值与 port 的数值比较——与 buildLeaf 对应字段的真实语义一致）。
func evalBracketNode(n *astNode, a bracketAsset) bool {
	switch n.kind {
	case "and":
		for _, c := range n.children {
			if !evalBracketNode(c, a) {
				return false
			}
		}
		return true
	case "or":
		for _, c := range n.children {
			if evalBracketNode(c, a) {
				return true
			}
		}
		return false
	case "leaf":
		e := n.expr
		switch strings.ToLower(e.Field) {
		case "domain":
			if e.Op == "!=" {
				return !strings.Contains(a.domain, e.Value)
			}
			if e.Op == "==" {
				return a.domain == e.Value
			}
			return strings.Contains(strings.ToLower(a.domain), strings.ToLower(e.Value))
		case "port":
			v, err := strconv.Atoi(e.Value)
			if err != nil {
				return false
			}
			switch e.Op {
			case ">", ">=":
				if e.Op == ">=" {
					return a.port >= v
				}
				return a.port > v
			case "<", "<=":
				if e.Op == "<=" {
					return a.port <= v
				}
				return a.port < v
			default:
				return a.port == v
			}
		default:
			return false
		}
	}
	return false
}

// genBracketExpr 随机生成含 AND/OR/括号的表达式（深度受控）。
func genBracketExpr(rnd *rand.Rand, depth int) string {
	pick := func(xs ...string) string { return xs[rnd.Intn(len(xs))] }
	domain := pick("alpha", "beta")
	port := pick("80", "8080", "443")
	atom := pick(
		"domain="+domain,
		"port>"+port,
		"port="+port,
	)
	if depth <= 0 {
		return atom
	}
	switch rnd.Intn(4) {
	case 0:
		return atom
	case 1:
		return genBracketExpr(rnd, depth-1) + " AND " + genBracketExpr(rnd, depth-1)
	case 2:
		return genBracketExpr(rnd, depth-1) + " OR " + genBracketExpr(rnd, depth-1)
	default:
		return "(" + genBracketExpr(rnd, depth-1) + ")"
	}
}

// 确定性用例：`A AND B OR C` 拼上类型/范围约束后，OR 分支不得把约束「漏」掉。
func TestDSLOrBracketKeepsConstraints(t *testing.T) {
	d := openTestDB(t)
	s := d.Assets()

	_ = seedBracketAssets(t, d, 4242)

	// 顶层 OR：beta 分支不受 typ 约束的旧病态 = 会数出 service 行。
	n, err := ParseDSL("domain=alpha AND port=80 OR domain=beta")
	if err != nil {
		t.Fatal(err)
	}
	where, _, err := buildDSLWhere(n)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(where, "(") || !strings.HasSuffix(where, ")") {
		t.Fatalf("顶层片段必须包括号，得到 %q", where)
	}

	// 经 CountDSL 走真实拼接路径（AND type=… AND $n=ANY(task_ids)）：
	// 结果集里每一行都必须同时满足 type 与范围——旧的裸拼接做不到。
	count, err := s.CountDSL("domain=alpha AND port=80 OR domain=beta", "subdomain", 4242)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := d.Query(`SELECT domain, port FROM assets WHERE type='subdomain' AND 4242 = ANY(task_ids)
		AND (domain ILIKE '%alpha%' AND port=80 OR domain ILIKE '%beta%')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	expect := 0
	for rows.Next() {
		expect++
	}
	if count != expect {
		t.Fatalf("括号化后 CountDSL=%d 应等于约束内语义行数 %d", count, expect)
	}
}

// 属性对照：SQL 求值 == 内存 AST 求值（≥200 组随机表达式）。
func TestDSLBracketPropertySemantics(t *testing.T) {
	d := openTestDB(t)
	s := d.Assets()
	assets := seedBracketAssets(t, d, 777)

	rnd := rand.New(rand.NewSource(20261125))
	for i := 0; i < 200; i++ {
		expr := genBracketExpr(rnd, 2)
		node, err := ParseDSL(expr)
		if err != nil {
			t.Fatalf("表达式 %q 解析失败: %v", expr, err)
		}
		want := 0
		for _, a := range assets {
			// CountDSL(expr, "subdomain", 777) 的语义 = AST(expr) AND type AND 范围。
			if evalBracketNode(node, a) && a.typ == "subdomain" && a.inScope {
				want++
			}
		}
		got, err := s.CountDSL(expr, "subdomain", 777)
		if err != nil {
			t.Fatalf("表达式 %q 求值失败: %v", expr, err)
		}
		if got != want {
			t.Fatalf("语义偏差：expr=%q SQL=%d AST=%d", expr, got, want)
		}
	}
}
