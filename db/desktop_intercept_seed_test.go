package db

import (
	"regexp"
	"strings"
	"testing"
)

// 桌面拦截分族的覆盖钉（0.11.9）：以 1.3.0 实测的 75 个真实工具名快照为全集，
// 钉死「三族 ∪ 高危 = 全集、族间互斥」——上游加新命令时本测试失败并提醒分族，
// 不会静默漏治理。同时验证四族正则确实按预期命中各自成员。

// families 返回【规则名 → 命令】（裸名，无 desktop_ 前缀）——键与
// desktopRuleSeeds 的 name 一一对应，正则级验证按它找族。顺序无关。
func families() map[string][]string {
	return map[string][]string{
		"[桌面] 观测类放行":   desktopObservationTools,
		"[桌面] 动作类审批":    desktopActionTools,
		"[桌面] 敏感读取审批": desktopSensitiveReadTools,
		"[桌面] 高危动作拒绝":  desktopHighRiskTools,
	}
}

// bare 剥掉平台全名的 mcp__<server>__ 前缀与 desktop_ 命令前缀，得到裸命令名。
func bare(full string) string {
	s := strings.TrimPrefix(full, "mcp__"+desktopServerName+"__")
	return strings.TrimPrefix(s, "desktop_")
}

func TestDesktopFamilyCoverage(t *testing.T) {
	observed := map[string]bool{}
	for _, n := range desktopObservedRealNames {
		observed[n] = true
	}
	if len(observed) != 75 {
		t.Fatalf("实测全集应 75 个，fixture 有 %d", len(observed))
	}
	covered := map[string]string{}
	for famName, cmds := range families() {
		for _, c := range cmds {
			if prev, dup := covered[c]; dup {
				t.Fatalf("命令 %s 同时出现在 %s 与 %s：族间互斥被破坏", c, prev, famName)
			}
			covered[c] = famName
		}
	}
	for _, n := range desktopObservedRealNames {
		if _, ok := covered[bare(n)]; !ok {
			t.Fatalf("实测工具 %s 未被任何族覆盖（上游加了新命令，分族缺失 → 静默漏治理）", n)
		}
	}
	for c := range covered {
		if !observed["desktop_"+c] {
			t.Fatalf("分族里的 %s 不在 1.3.0 实测全集内（上游改名/删命令，名单陈旧）", c)
		}
	}
}

// 四族正则按 desktopFamilyPattern 编译后，对各自成员命中、对实测全集内其它族
// 成员不命中（族间互斥的正则级验证）；deny 规则必须排第一且优先级最高。
func TestDesktopFamilyPatterns(t *testing.T) {
	seed := desktopRuleSeeds()
	type compiled struct {
		action  string
		pattern *regexp.Regexp
		family  []string
	}
	res := make([]compiled, 0, len(seed))
	for _, r := range seed {
		fam, ok := families()[r.name]
		if !ok {
			t.Fatalf("规则 %q 找不到对应分族（规则名与 families() 键必须一一对应）", r.name)
		}
		res = append(res, compiled{action: r.action, pattern: regexp.MustCompile(r.pattern), family: fam})
	}
	if res[0].action != "deny" {
		t.Fatal("deny 规则必须排第一（多命中时裁决取先命中者）")
	}
	for i, c := range res {
		for _, m := range c.family {
			full := desktopToolFull(m)
			if !c.pattern.MatchString(full) {
				t.Fatalf("%s 的正则未命中本族成员 %s（pattern=%s）", res[i].action, full, c.pattern)
			}
		}
	}
	for i, c := range res {
		for _, full := range desktopObservedRealNames {
			member := false
			for _, m := range c.family {
				if bare(full) == m {
					member = true
					break
				}
			}
			if member {
				continue
			}
			if c.pattern.MatchString(full) {
				t.Fatalf("%s 正则误命中非本族成员 %s", res[i].action, full)
			}
		}
	}
	// 优先级单调：deny(100) > sensitive(90) > action(50) > allow(10)。
	priorities := make([]int, len(seed))
	for i, r := range seed {
		priorities[i] = r.priority
	}
	for i := 1; i < len(priorities); i++ {
		if priorities[i] >= priorities[i-1] {
			t.Fatalf("规则优先级应严格降序: %v", priorities)
		}
	}
}
