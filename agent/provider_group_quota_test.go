package agent

import (
	"testing"

	"github.com/Autumn-27/artex/llmrec"
)

// 平台自己的「群组预算拒绝」绝不能被判成供应商额度耗尽。
//
// 判成供应商额度耗尽的后果：quotaAwareTransport 把 429 归一化成 402、任务路由器
// 把当前 profile 标成 quota_exhausted 并切走 —— 但供应商其实还好好的，别的任务
// 因此也失去这个供应商（task_llm_profiles 被污染）。
func TestIsQuotaExhaustedMessageIgnoresGroupQuotaRefusal(t *testing.T) {
	t.Parallel()
	if IsQuotaExhaustedMessage(llmrec.GroupQuotaRefusal(nil).Error()) {
		t.Fatal("群组预算拒绝被误判成供应商额度耗尽")
	}
}

// 双保险：即使标记被剥掉，拒绝文案本身也必须不撞供应商额度 markers。
//
// 这条锁的是「文案层」的措施：群组预算用「预算」措辞，供应商额度用「额度」
// 措辞，两者字面不重叠。
func TestIsQuotaExhaustedMessageIgnoresGroupQuotaRefusalCopyAlone(t *testing.T) {
	t.Parallel()
	if IsQuotaExhaustedMessage(llmrec.GroupQuotaRefusalCopy) {
		t.Fatalf("群组预算拒绝文案撞上供应商额度 markers: %q", llmrec.GroupQuotaRefusalCopy)
	}
}

// 供应商自己的额度耗尽文案必须**仍然**被识别 —— 群组预算的豁免不能顺手把
// 供应商额度也放过去（那会让真正的额度耗尽不再 failover）。
func TestIsQuotaExhaustedMessageStillDetectsProviderQuota(t *testing.T) {
	t.Parallel()
	for _, message := range []string{
		"额度不足",
		"额度已用尽",
		"欠费",
		"余额不足",
		"insufficient_quota",
		"billing_hard_limit_reached",
	} {
		if !IsQuotaExhaustedMessage(message) {
			t.Errorf("供应商额度文案未被识别: %q", message)
		}
	}
}
