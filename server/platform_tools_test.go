package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSkillTools functionally exercises create_skill + update_skill_file against a
// temp skill dir (no DB needed — these tools only touch the filesystem).
func TestSkillTools(t *testing.T) {
	dir := t.TempDir()
	s := &Server{skillDir: dir}

	// create_skill writes a spec-compliant SKILL.md.
	in, _ := json.Marshal(map[string]any{
		"name": "my-skill", "description": "does X", "instructions": "## do\n1. a",
	})
	res, err := s.toolCreateSkill().Call(context.Background(), in, nil)
	if err != nil || res.IsError {
		t.Fatalf("create_skill: err=%v result=%+v", err, res)
	}
	md, err := os.ReadFile(filepath.Join(dir, "my-skill", "SKILL.md"))
	if err != nil {
		t.Fatalf("SKILL.md not written: %v", err)
	}
	if !strings.Contains(string(md), "name: my-skill") || !strings.Contains(string(md), "description: does X") || !strings.Contains(string(md), "1. a") {
		t.Fatalf("SKILL.md content:\n%s", md)
	}

	// creating the same skill again fails.
	if r, _ := s.toolCreateSkill().Call(context.Background(), in, nil); !r.IsError {
		t.Fatal("duplicate create should error")
	}

	// invalid name rejected.
	bad, _ := json.Marshal(map[string]any{"name": "Bad Name", "description": "x"})
	if r, _ := s.toolCreateSkill().Call(context.Background(), bad, nil); !r.IsError {
		t.Fatal("invalid skill name should error")
	}

	// update_skill_file writes a nested script; path traversal is rejected.
	up, _ := json.Marshal(map[string]any{"name": "my-skill", "file": "scripts/run.py", "content": "print(1)"})
	if r, err := s.toolUpdateSkillFile().Call(context.Background(), up, nil); err != nil || r.IsError {
		t.Fatalf("update_skill_file: err=%v result=%+v", err, r)
	}
	got, err := os.ReadFile(filepath.Join(dir, "my-skill", "scripts", "run.py"))
	if err != nil || string(got) != "print(1)" {
		t.Fatalf("nested file not written: %v %q", err, got)
	}
	evil, _ := json.Marshal(map[string]any{"name": "my-skill", "file": "../escape.txt", "content": "x"})
	if r, _ := s.toolUpdateSkillFile().Call(context.Background(), evil, nil); !r.IsError {
		t.Fatal("path traversal should be rejected")
	}
}

// validateMCPDefinition 是 guard 硬拦之外的兜底层，用例要覆盖两类真实绕过动机：
// 用 stdio 包执行器拿下 root 代码执行，或用 http/sse 把平台变成已认证 SSRF 原语。
func TestValidateMCPDefinitionRejectsRisky(t *testing.T) {
	cases := []struct {
		desc string
		in   mcpToolInput
	}{
		{"stdio 用 npx", mcpToolInput{Transport: "stdio", Command: "npx"}},
		{"stdio 用 bash", mcpToolInput{Transport: "stdio", Command: "/bin/bash"}},
		{"stdio 用 python3", mcpToolInput{Transport: "stdio", Command: "python3"}},
		{"stdio 命令为空", mcpToolInput{Transport: "stdio"}},
		{"file 协议", mcpToolInput{Transport: "http", URL: "file:///etc/passwd"}},
		{"缺 host", mcpToolInput{Transport: "http", URL: "http://"}},
		{"内嵌 userinfo", mcpToolInput{Transport: "http", URL: "http://u:p@172.19.0.1:8001/mcp"}},
		{"未知 transport", mcpToolInput{Transport: "grpc", URL: "http://x"}},
	}
	for _, c := range cases {
		if _, err := validateMCPDefinition(&c.in); err == nil {
			t.Fatalf("%s 应被拒: %+v", c.desc, c.in)
		}
	}
}

func TestValidateMCPDefinitionAllowsNormal(t *testing.T) {
	for _, u := range []string{
		"http://172.19.0.1:8001/mcp",
		"https://mcp.example.com/sse",
		"http://172.19.0.1:9879/sse",
	} {
		in := mcpToolInput{Transport: "http", URL: u}
		if tr, err := validateMCPDefinition(&in); err != nil || tr != "" {
			t.Fatalf("正常 URL %s 被误伤: %v", u, err)
		}
	}
	in := mcpToolInput{Transport: "stdio", Command: "/usr/bin/playwright-mcp"}
	got, err := validateMCPDefinition(&in)
	if err != nil {
		t.Fatalf("本地绝对路径 stdio 应放行: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("命令应规范化为绝对路径，得到 %q", got)
	}
}
