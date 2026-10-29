package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// 本文件钉住【范围判定只有一个实现】这件事。
//
// 为什么需要它：范围判定现在有两个调用面 ——
//  ① 工具面（agent 的 list_assets 等读工具）；
//  ② 平台 API 面（server 的资产读接口；后续阶段接入，它会调用同一个 db 包）。
// 两边若各自实现一遍范围规则，它们就会漂移：工具面放行了一个资产，API 面拒绝它
// （或反过来），而没有任何东西会变红。Sir 的原话是「比『代码 review 保证不漂移』更
// 可靠 —— 用测试钉住」。
//
// 为什么入口是一张【表】：平台 API 面还没接入（它属于后续阶段）。把它做成表里的一项，
// 接入时只要追加一项、断言逻辑一个字都不用改。反过来，如果把两个入口写死在两段
// 并列的代码里，接第三个入口时就得重写整个用例 —— 那正是「入口会漂移」的开始。
//
// 为什么测试放在 agent 包（而不是 db 包）：入口 (a) 必须是【真的工具】。
// 只在 db 包里比「GetByIDsInScope vs ScopedAssetIDs」，能发现 db 内部的漂移，却
// 发现不了「agent 侧自己另写了一套范围 SQL」——那才是最容易发生的那种漂移。

// scopeConsistencyFixture 是本文件的夹具：两个任务、互不相交的范围，各一枚资产。
type scopeConsistencyFixture struct {
	db    *db.DB
	taskA int64
	taskB int64
	expA  int64

	inHost   string
	outHost  string
	inAsset  int64
	outAsset int64
}

func newScopeConsistencyFixture(t *testing.T) *scopeConsistencyFixture {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("没有数据库配置（%v）", err)
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres 不可用（%v）", err)
	}
	t.Cleanup(func() { d.Close() })

	taskA, err := d.CreateTask("scope 一致性 A", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskB, err := d.CreateTask("scope 一致性 B", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(taskA.ID); _ = d.DeleteTask(taskB.ID) })

	stamp := time.Now().UnixNano()
	fx := &scopeConsistencyFixture{
		db:      d,
		taskA:   taskA.ID,
		taskB:   taskB.ID,
		expA:    taskA.ExplorationID,
		inHost:  fmt.Sprintf("scope-ok-in-%d.example.test", stamp),
		outHost: fmt.Sprintf("scope-ok-out-%d.example.test", stamp),
	}

	// 声明范围：直接写 task_scope 行（root_domain 必须与资产行的 root_domain 列
	// 逐字相等 —— 范围 CTE 的匹配条件就是 a.root_domain = ts.domain）。
	for _, sc := range []struct {
		taskID int64
		domain string
	}{
		{taskA.ID, fx.inHost},
		{taskB.ID, fx.outHost},
	} {
		if _, err := d.Exec(`INSERT INTO task_scope(task_id,kind,domain,source) VALUES ($1,'root_domain',$2,'manual')
ON CONFLICT DO NOTHING`, sc.taskID, sc.domain); err != nil {
			t.Fatalf("声明范围失败：%v", err)
		}
	}

	fx.inAsset = tier1InsertAsset(t, d, fx.inHost)
	fx.outAsset = tier1InsertAsset(t, d, fx.outHost)
	t.Cleanup(func() {
		d.Exec(`DELETE FROM assets WHERE id=ANY($1::bigint[])`, []int64{fx.inAsset, fx.outAsset}) //nolint:errcheck
	})
	return fx
}

// scopeEntry 是一个范围判定入口。
type scopeEntry struct {
	// name 是入口名：失败信息里必须能看出【是谁】判的。
	name string
	// reachable 判定该入口下 assetID 对 taskID 是否可达。
	reachable func(t *testing.T, taskID, assetID int64) bool
}

// scopeCase 是一组 (taskID, assetID) 与它应有的判定结果。
type scopeCase struct {
	name    string
	taskID  int64
	assetID int64
	want    bool
}

// TestScopeDecisionConsistentAcrossEntries 钉住「范围判定只有一个实现」。
//
// 每个用例跑遍【所有入口】，并要求：
//  1. 各入口的判定逐对一致 —— 「一个说可达、另一个说不可达」就是漂移，直接失败；
//  2. 各入口的判定与用例自己写下的期望一致 —— 全部入口一起错也算失败。
//
// 期望值里最要紧的是 taskID<=0 那一档：没有任务上下文就没有授权边界，此时两个入口
// 都必须判「不可达」（fail-closed），绝不能回退到「全都给」。
func TestScopeDecisionConsistentAcrossEntries(t *testing.T) {
	fx := newScopeConsistencyFixture(t)

	cases := []scopeCase{
		{name: "范围内的资产", taskID: fx.taskA, assetID: fx.inAsset, want: true},
		{name: "范围外的资产", taskID: fx.taskA, assetID: fx.outAsset, want: false},
		{name: "另一个任务的资产", taskID: fx.taskB, assetID: fx.inAsset, want: false},
		{name: "无任务上下文", taskID: 0, assetID: fx.inAsset, want: false},
		{name: "负任务号", taskID: -1, assetID: fx.inAsset, want: false},
	}

	// 入口表。后续阶段平台 API 面接入时，在这里【追加一项】即可：它会调用同一个
	// db 包的 (*AssetStore).ScopedAssetIDs（范围规则只有那一份，见 db/scope.go），
	// 断言逻辑一个字都不用改。现在【不】放一个假的 API 入口 —— 假的入口只会让这条
	// 测试变成永远为真的空断言。
	entries := []scopeEntry{
		{
			name: "工具面(list_assets)",
			reachable: func(t *testing.T, taskID, assetID int64) bool {
				tools := agent.NewToolSet(fx.db.Exploration(fx.expA), "planner")
				tools.SetTaskID(taskID)
				tools.SetAssetStore(fx.db.Assets(), fx.db.Companies())
				out := tier1Call(t, tier1Tool(t, tools.AllDomainTools(), "list_assets"), context.Background(),
					map[string]any{"id": assetID})
				var payload struct {
					Assets []struct {
						ID int64 `json:"id"`
					} `json:"assets"`
				}
				if err := json.Unmarshal([]byte(out), &payload); err != nil {
					t.Fatalf("list_assets 输出不是 JSON：%v；原始输出：%s", err, out)
				}
				for _, a := range payload.Assets {
					if a.ID == assetID {
						return true
					}
				}
				return false
			},
		},
		{
			name: "db 包入口(ScopedAssetIDs)",
			reachable: func(t *testing.T, taskID, assetID int64) bool {
				ids, err := fx.db.Assets().ScopedAssetIDs(taskID, []int64{assetID})
				if err != nil {
					t.Fatalf("ScopedAssetIDs 失败：%v", err)
				}
				for _, id := range ids {
					if id == assetID {
						return true
					}
				}
				return false
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var decided *bool
			var decidedBy string
			for _, e := range entries {
				got := e.reachable(t, tc.taskID, tc.assetID)
				if decided == nil {
					value := got
					decided = &value
					decidedBy = e.name
					continue
				}
				if got != *decided {
					t.Fatalf("范围判定在两个入口之间漂移：%s 判为可达=%v，%s 判为可达=%v"+
						"（taskID=%d assetID=%d）—— 两个入口必须共用同一套范围规则",
						decidedBy, *decided, e.name, got, tc.taskID, tc.assetID)
				}
			}
			if decided == nil {
				t.Fatal("入口表是空的：用例什么都没验")
			}
			if *decided != tc.want {
				t.Fatalf("taskID=%d assetID=%d 的可达性应为 %v，实际 %v（入口 %s）",
					tc.taskID, tc.assetID, tc.want, *decided, decidedBy)
			}
		})
	}
}
