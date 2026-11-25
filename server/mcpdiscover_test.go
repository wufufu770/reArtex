package server

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// A-07（0.15.81）：stdio MCP 命令校验从 base 名字比对改为「名字模式 + 真实路径」
// 双规则。验收夹具：版本化解释器、绝对路径形态、env 前缀、软链改名——全部拒绝；
// 合法本地二进制通过并返回真实绝对路径。
func TestValidateStdioCommand(t *testing.T) {
	bin := t.TempDir()
	target := filepath.Join(bin, "python3.12") // 目标名带版本：软链解析后现形
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(bin, "mypy") // 改名软链：base 是 mypy，名字层看不出
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("无法创建软链: %v", err)
	}
	legit := filepath.Join(bin, "my-mcp-server")
	if err := os.WriteFile(legit, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	exporter := filepath.Join(bin, "node-exporter") // node 前缀但非解释器：名字模式不得过宽
	if err := os.WriteFile(exporter, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("拒绝", func(t *testing.T) {
		for _, cmd := range []string{
			"python3.11",          // 版本化裸名（旧版漏网）
			"/usr/bin/python3.11", // 绝对路径形态
			"env python3",         // env 前缀
			"env -i PYTHONDONTWRITEBYTECODE=1 python", // env 旗标+赋值+解释器
			"npx", "uvx --version", "docker",
			alias, // 改名软链：真实路径解析后指向 python3.12
			"env FOO=1 " + alias,
		} {
			if _, err := validateStdioCommand(cmd); err == nil {
				t.Fatalf("命令 %q 应被拒绝", cmd)
			}
		}
	})

	t.Run("放行", func(t *testing.T) {
		for _, tc := range []struct{ cmd, want string }{
			{legit, legit},       // 合法本地二进制：返回真实绝对路径
			{exporter, exporter}, // node 前缀但非解释器
		} {
			got, err := validateStdioCommand(tc.cmd)
			if err != nil {
				t.Fatalf("命令 %q 不应被拒绝: %v", tc.cmd, err)
			}
			if got != tc.want {
				t.Fatalf("返回值应为真实绝对路径 %q，得到 %q", tc.want, got)
			}
		}
	})

	t.Run("裸名经 PATH 解析", func(t *testing.T) {
		t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
		got, err := validateStdioCommand("my-mcp-server")
		if err != nil {
			t.Fatalf("裸名解析失败: %v", err)
		}
		if got != legit {
			t.Fatalf("应解析为 %q，得到 %q", legit, got)
		}
		if _, err := validateStdioCommand("no-such-binary-xyz"); err == nil {
			t.Fatal("PATH 里解析不到的裸名应被拒绝")
		}
	})

	t.Run("软链指向系统解释器", func(t *testing.T) {
		// 真实环境形态：/tmp/evil → /usr/bin/python3。目标存在与否都会在
		// 名字层或真实路径层被拦——按是否存在分两种断言。
		link := filepath.Join(t.TempDir(), "evil")
		if err := os.Symlink("/usr/bin/python3", link); err != nil {
			t.Skipf("无法创建软链: %v", err)
		}
		if _, err := os.Stat("/usr/bin/python3"); err == nil {
			if _, err := validateStdioCommand(link); err == nil {
				t.Fatalf("软链指向系统解释器 %q 应被拒绝", link)
			}
		} else if _, err := validateStdioCommand(link); err == nil {
			t.Fatalf("软链（目标缺失）应拒绝而非放行: %q", link)
		}
	})
}

// 存量重验：登记过的 stdio 行如果命令已失效（二进制被删），启动重验必须
// 记警告并禁用（enabled=false），而不是静默放行等拨号失败冷却。
// onlyNameStore 把 ListMCP 收窄到指定名字的行：重验会【禁用】它看到的每一行，
// 而测试库是整包共享的（种子里就有指向本机不存在二进制的 stdio 行），
// 不收窄会把共享夹具一起改掉。
type onlyNameStore struct {
	*db.DB
	name string
}

func (s onlyNameStore) ListMCP() ([]*db.MCPServer, error) {
	rows, err := s.DB.ListMCP()
	var out []*db.MCPServer
	for _, r := range rows {
		if r.Name == s.name {
			out = append(out, r)
		}
	}
	return out, err
}

func TestReverifyStdioMCPsDisablesBroken(t *testing.T) {
	pg := trustTestDB(t)
	name := "reverify-broken-" + rngSuffix()
	m := &db.MCPServer{
		Name: name, Transport: "stdio",
		Command: filepath.Join(t.TempDir(), "gone-binary"), // 文件不存在 → 重验必挂
		Args:    []byte("[]"), Env: []byte("{}"), Enabled: true,
	}
	id, err := pg.SaveMCP(m)
	if err != nil {
		t.Fatalf("预置存量行失败: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteMCP(id) })

	reverifyStdioMCPs(onlyNameStore{DB: pg, name: name})

	rows, err := pg.ListMCP()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Name != name {
			continue
		}
		if r.Enabled {
			t.Fatal("重验不通过的存量 stdio MCP 应被禁用，仍处于启用态")
		}
		return
	}
	t.Fatalf("预置行 %q 未找到", name)
}

// 合法的存量 stdio 行重验后保持启用（重验只打不合规行）。
func TestReverifyStdioMCPsKeepsValid(t *testing.T) {
	pg := trustTestDB(t)
	bin := t.TempDir()
	legit := filepath.Join(bin, "my-mcp-server")
	if err := os.WriteFile(legit, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	name := "reverify-valid-" + rngSuffix()
	m := &db.MCPServer{
		Name: name, Transport: "stdio", Command: legit,
		Args: []byte("[]"), Env: []byte("{}"), Enabled: true,
	}
	id, err := pg.SaveMCP(m)
	if err != nil {
		t.Fatalf("预置存量行失败: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteMCP(id) })

	reverifyStdioMCPs(onlyNameStore{DB: pg, name: name})

	rows, err := pg.ListMCP()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Name != name {
			continue
		}
		if !r.Enabled {
			t.Fatal("合法存量行不应被重验禁用")
		}
		if !strings.HasPrefix(r.Command, "/") {
			t.Fatalf("重验不应改写命令字面量，得到 %q", r.Command)
		}
		return
	}
	t.Fatalf("预置行 %q 未找到", name)
}

// rngSuffix 给测试行名加唯一后缀：同一测试库被整包用例共享，残留行会互相干扰。
func rngSuffix() string {
	return strconv.FormatInt(time.Now().UnixNano(), 10)
}
