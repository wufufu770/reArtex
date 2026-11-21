package sod

// engagementElevate 单测：任务授权面三态收紧纪律（0.15.26）。
// 词表/档位语义改一处这里就红（与 db/engagement_test.go 互为表里）。
import (
	"context"
	"testing"
)

func engagementChain(profile string, approved *bool) *Chain {
	return &Chain{deps: ChainDeps{
		EngagementProfileOf: func(int64) string { return profile },
		ElevateAsk: func(ctx context.Context, req AskRequest) (bool, string, error) {
			if approved != nil {
				*approved = true
			}
			return true, "human", nil
		},
	}}
}

func TestEngagementElevateDefaultNoTighten(t *testing.T) {
	// 默认档（authorized_active / 未注入 / 空串）零收紧——现状行为不变。
	cases := []*Chain{
		{deps: ChainDeps{}},
		engagementChain("", nil),
		engagementChain("authorized_active", nil),
		engagementChain("未知档位", nil), // 未知档位运行期不做创造性解释
	}
	cls := ClassResult{Kind: ClassTargetKnown, Hosts: []string{"target.example"}}
	markers := map[string]string{}
	for i, c := range cases {
		block, _ := c.engagementElevate(t.Context(), chainCallFor(1, "Bash", `{"command":"curl x"}`), cls, &markers)
		if block {
			t.Fatalf("case %d：默认档不得收紧", i)
		}
	}
}

func TestEngagementElevatePassiveReconOnEgress(t *testing.T) {
	// passive_recon：任何出网形态（远端主机/GET/DNS/目标写）都要人工确认。
	egress := []ClassResult{
		{Kind: ClassTargetKnown, Hosts: []string{"target.example"}},
		{Kind: ClassIntelSource, HTTPGet: true},
		{Kind: ClassIntelSource, DNSQuery: true},
		{Kind: ClassTargetKnown, TargetWrite: true, Hosts: []string{"target.example"}},
	}
	for i, cls := range egress {
		var approved bool
		c := engagementChain("passive_recon", &approved)
		markers := map[string]string{}
		block, _ := c.engagementElevate(t.Context(), chainCallFor(1, "Bash", `{"command":"curl x"}`), cls, &markers)
		// ask 通路必须被触发（approved），批准后继续放行（block=false = 不 deny）。
		if !approved || block {
			t.Fatalf("case %d：passive_recon 出网动作应先经人工确认（ask 触发=%v deny=%v）", i, approved, block)
		}
		if markers["engagement"] != "passive_recon:approved" {
			t.Fatalf("case %d：审计 marker 缺失: %q", i, markers["engagement"])
		}
	}
	// 平台内动作（无出网形态）照常放行——落库/读图不打断 recon 流程。
	local := ClassResult{Kind: ClassLocalSafe}
	c := engagementChain("passive_recon", nil)
	if block, _ := c.engagementElevate(t.Context(), chainCallFor(1, "record_fact", `{}`), local, &map[string]string{}); block {
		t.Fatal("passive_recon 平台内动作不得收紧")
	}
}

func TestEngagementElevateStrictManual(t *testing.T) {
	// strict_manual：除平台内安全动作外一律先经人工确认。
	nonSafe := []ClassResult{
		{Kind: ClassTargetKnown, Hosts: []string{"target.example"}},
		{Kind: ClassUnclear},
		{Kind: ClassOutsideWrite, OutsidePaths: []string{"/etc"}},
	}
	for i, cls := range nonSafe {
		var approved bool
		c := engagementChain("strict_manual", &approved)
		markers := map[string]string{}
		block, _ := c.engagementElevate(t.Context(), chainCallFor(1, "Bash", `{"command":"ls"}`), cls, &markers)
		// ask 通路必须被触发（批准后放行；拒答/超时路径由 Declined 用例覆盖）。
		if !approved || block {
			t.Fatalf("case %d：strict_manual 非平台内安全动作应先确认（ask 触发=%v deny=%v）", i, approved, block)
		}
	}
	// 平台内安全动作放行（问人纯属骚扰）。
	for i, cls := range []ClassResult{{Kind: ClassBenign}, {Kind: ClassLocalSafe}} {
		c := engagementChain("strict_manual", nil)
		if block, _ := c.engagementElevate(t.Context(), chainCallFor(1, "list_assets", `{}`), cls, &map[string]string{}); block {
			t.Fatalf("case %d：strict_manual 平台内安全动作不得收紧", i)
		}
	}
}

func TestEngagementElevateDeclinedFailsClosed(t *testing.T) {
	// 人工拒答/超时 ⇒ deny（fail-closed，与种类 ask 同语义），marker 记 declined。
	c := &Chain{deps: ChainDeps{
		EngagementProfileOf: func(int64) string { return "strict_manual" },
		ElevateAsk:          func(ctx context.Context, req AskRequest) (bool, string, error) { return false, "", nil },
	}}
	cls := ClassResult{Kind: ClassTargetKnown, Hosts: []string{"target.example"}}
	markers := map[string]string{}
	block, note := c.engagementElevate(t.Context(), chainCallFor(1, "Bash", `{"command":"curl x"}`), cls, &markers)
	if !block {
		t.Fatal("拒答后应返回 block")
	}
	if markers["engagement"] != "strict_manual:declined" {
		t.Fatalf("marker 应记 declined: %q", markers["engagement"])
	}
	if note == "" {
		t.Fatal("deny note 不得为空（审计要能回放为什么）")
	}
}
