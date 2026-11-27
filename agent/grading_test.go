package agent

import (
	"math"
	"os"
	"strings"
	"testing"
)

func TestComputeGradeFullMarksIsS(t *testing.T) {
	got := ComputeGrade(GradeInput{
		GoalsMet: 4, GoalsTotal: 4,
		VerifiedFindings: 12, TotalFindings: 12,
		CoveredAssets: 3, ScopedAssets: 3,
		EvidenceCompleteFindings: 12,
	})
	if got.Grade != GradeS || got.Score != 100 {
		t.Fatalf("全满应是 100 S，得到 %v %v", got.Score, got.Grade)
	}
	want := "4/4 目标达成，12/12 条发现已验证，覆盖率 100%，证据完整率 100%"
	if got.Summary != want {
		t.Fatalf("摘要\n got %q\nwant %q", got.Summary, want)
	}
}

func TestComputeGradeAcceptanceExampleIsA(t *testing.T) {
	// 3/4、9/12、82/100、9/12 → 0.75×40 + 0.75×30 + 0.82×20 + 0.75×10 = 76.4 → A
	got := ComputeGrade(GradeInput{
		GoalsMet: 3, GoalsTotal: 4,
		VerifiedFindings: 9, TotalFindings: 12,
		CoveredAssets: 82, ScopedAssets: 100,
		EvidenceCompleteFindings: 9,
	})
	if got.Grade != GradeA {
		t.Fatalf("76.4 应是 A，得到 %v %v", got.Score, got.Grade)
	}
	if math.Abs(got.Score-76.4) > 1e-9 {
		t.Fatalf("分数应是 76.4，得到 %v", got.Score)
	}
	want := "3/4 目标达成，9/12 条发现已验证，覆盖率 82%，证据完整率 75%"
	if got.Summary != want {
		t.Fatalf("摘要\n got %q\nwant %q", got.Summary, want)
	}
}

func TestComputeGradeThresholds(t *testing.T) {
	cases := []struct {
		name  string
		in    GradeInput
		grade string
		score float64
	}{
		{
			name: "exactly 90 is S",
			in: GradeInput{
				GoalsMet: 4, GoalsTotal: 4,
				VerifiedFindings: 12, TotalFindings: 12,
				CoveredAssets: 100, ScopedAssets: 100,
				EvidenceCompleteFindings: 0,
			},
			grade: GradeS, score: 90,
		},
		{
			name: "exactly 75 is A",
			in: GradeInput{
				GoalsMet: 4, GoalsTotal: 4,
				VerifiedFindings: 10, TotalFindings: 10,
				CoveredAssets: 1, ScopedAssets: 4,
				EvidenceCompleteFindings: 0,
			},
			grade: GradeA, score: 75,
		},
		{
			name: "exactly 60 is B",
			in: GradeInput{
				GoalsMet: 1, GoalsTotal: 2,
				VerifiedFindings: 10, TotalFindings: 10,
				CoveredAssets: 1, ScopedAssets: 2,
				EvidenceCompleteFindings: 0,
			},
			grade: GradeB, score: 60,
		},
		{
			name: "below 60 is C",
			in: GradeInput{
				GoalsMet: 1, GoalsTotal: 2,
				VerifiedFindings: 0, TotalFindings: 10,
				CoveredAssets: 1, ScopedAssets: 2,
				EvidenceCompleteFindings: 0,
			},
			grade: GradeC, score: 30,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeGrade(tc.in)
			if got.Grade != tc.grade || math.Abs(got.Score-tc.score) > 1e-9 {
				t.Fatalf("got %v %s, want %v %s", got.Score, got.Grade, tc.score, tc.grade)
			}
		})
	}
}

func TestComputeGradeZeroDenominatorIsNotFullMarks(t *testing.T) {
	got := ComputeGrade(GradeInput{})
	if got.Grade != GradeC || got.Score != 0 {
		t.Fatalf("分母全 0 应是 0 分 C，得到 %v %s", got.Score, got.Grade)
	}
	want := "0/0 目标达成，0/0 条发现已验证，覆盖率 0%，证据完整率 0%（无目标按 0 计；无发现按 0 计；无范围资产按 0 计）"
	if got.Summary != want {
		t.Fatalf("摘要\n got %q\nwant %q", got.Summary, want)
	}
	// 负分母同样按 0，不 panic。
	neg := ComputeGrade(GradeInput{GoalsMet: 1, GoalsTotal: -1, VerifiedFindings: 1, TotalFindings: -3, CoveredAssets: 1, ScopedAssets: -2})
	if neg.Score != 0 || neg.Grade != GradeC {
		t.Fatalf("负分母应是 0 分 C，得到 %v %s", neg.Score, neg.Grade)
	}
}

func TestComputeGradeIsDeterministic(t *testing.T) {
	in := GradeInput{GoalsMet: 3, GoalsTotal: 4, VerifiedFindings: 9, TotalFindings: 12, CoveredAssets: 82, ScopedAssets: 100, EvidenceCompleteFindings: 9}
	a := ComputeGrade(in)
	b := ComputeGrade(in)
	if a != b {
		t.Fatalf("同一输入两次结果不同：%+v vs %+v", a, b)
	}
}

func TestComputeGradeDoesNotClampNumerator(t *testing.T) {
	// 公式按卡面不封顶。分子大于分母时分数可以超过 100，等级仍是 S。
	got := ComputeGrade(GradeInput{GoalsMet: 2, GoalsTotal: 1, VerifiedFindings: 1, TotalFindings: 1, CoveredAssets: 1, ScopedAssets: 1, EvidenceCompleteFindings: 1})
	if got.Score != 140 || got.Grade != GradeS {
		t.Fatalf("2/1 目标应把分数推到 140 S，得到 %v %s", got.Score, got.Grade)
	}
}

func TestCountEvidenceCompleteUsesFindingGate(t *testing.T) {
	findings := []GradeFinding{
		{Summary: "SQL 注入可读取他人数据", Evidence: "GET /api?id=1 HTTP/1.1\n200 OK"},
		{Summary: "SQL 注入可读取他人数据", Evidence: "   "},
		{Summary: "这不是漏洞", Evidence: "GET /api?id=1 HTTP/1.1\n200 OK"},
		{Summary: "未验证的注入点", Evidence: "GET /api?id=1 HTTP/1.1\n200 OK"},
	}
	if n := CountEvidenceComplete(findings); n != 1 {
		t.Fatalf("只有第一条过门，得到 %d", n)
	}
	if n := CountEvidenceComplete(nil); n != 0 {
		t.Fatalf("空列表应是 0，得到 %d", n)
	}
}

func TestGradingSourceDoesNotCallLLM(t *testing.T) {
	src, err := os.ReadFile("grading.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	// 只查导入和调用形态。函数名 CountEvidenceComplete 含 Complete，不能拿子串误伤。
	for _, bad := range []string{`"github.com/Autumn-27/artex/llm"`, "llm.", "openai", "anthropic", "net/http", ".Complete("} {
		if strings.Contains(text, bad) {
			t.Fatalf("grading.go 不应出现 %q", bad)
		}
	}
}

func TestValidGrade(t *testing.T) {
	for _, ok := range []string{GradeS, GradeA, GradeB, GradeC} {
		if !ValidGrade(ok) {
			t.Fatalf("%s 应合法", ok)
		}
	}
	for _, bad := range []string{"", "s", "D", "medium", "auto"} {
		if ValidGrade(bad) {
			t.Fatalf("%q 不应合法", bad)
		}
	}
}
