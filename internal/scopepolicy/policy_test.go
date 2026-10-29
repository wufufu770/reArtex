package scopepolicy

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestPolicyDefaults pins the code-level scope policy. These values are the
// ceiling for every scope decision in the platform: they live in code on
// purpose (a DB rule row can be deleted, a settings key can be hot-changed by
// anyone holding a JWT), so a change here is always a deliberate act.
func TestPolicyDefaults(t *testing.T) {
	if Policy.PlatformScopeMode != ScopeEmpty {
		t.Fatalf("PlatformScopeMode=%q want %q（无任务上下文默认 fail-closed）", Policy.PlatformScopeMode, ScopeEmpty)
	}
	if Policy.MaxAgentAddedScope != 8 {
		t.Fatalf("MaxAgentAddedScope=%d want 8（单次任务的扩张预算）", Policy.MaxAgentAddedScope)
	}
	want := []string{"subdomain", "ip", "cidr", "icp", "keyword"}
	if len(Policy.MaxAgentAddedKinds) != len(want) {
		t.Fatalf("MaxAgentAddedKinds=%v want %v", Policy.MaxAgentAddedKinds, want)
	}
	for i, kind := range want {
		if Policy.MaxAgentAddedKinds[i] != kind {
			t.Fatalf("MaxAgentAddedKinds[%d]=%q want %q", i, Policy.MaxAgentAddedKinds[i], kind)
		}
	}
}

// TestPolicyExcludesTreePullingKinds guards the deliberate exclusion:
// root_domain / company pull in the whole asset tree, so an agent may never
// widen the scope into them on its own — that needs human confirmation.
func TestPolicyExcludesTreePullingKinds(t *testing.T) {
	for _, forbidden := range []string{"root_domain", "company"} {
		for _, kind := range Policy.MaxAgentAddedKinds {
			if kind == forbidden {
				t.Fatalf("MaxAgentAddedKinds 不得包含 %q（会把整棵资产树拉进来）", forbidden)
			}
		}
	}
}

// TestPackageIsLeaf guards the hard import-cycle constraint: scopepolicy must
// not import any other artex package. db / agent / server all have to import
// it, so an import of db or agent from here would make the build impossible.
func TestPackageIsLeaf(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("parse import in %s: %v", name, err)
			}
			if strings.HasPrefix(path, "github.com/Autumn-27/artex") {
				t.Fatalf("%s 不得 import 其它 artex 包（scopepolicy 必须是叶子包），发现 %q", name, path)
			}
		}
	}
}
