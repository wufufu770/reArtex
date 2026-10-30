package llmrec

import (
	"errors"
	"strings"
	"testing"
)

// 平台自己抛出的「群组预算拒绝」必须能被自己认出来。
//
// 为什么要有这个标记：群组预算用尽与供应商额度耗尽是**两件相反的事** ——
// 前者只要停派发，后者要切走 profile。若两者混为一谈，当前 profile 会被误标成
// quota_exhausted 并切走，既污染 task_llm_profiles（别的任务也失去这个供应商），
// 又让群组预算的语义被埋进供应商额度里。
func TestIsGroupQuotaMessageRecognizesOwnRefusal(t *testing.T) {
	t.Parallel()
	err := GroupQuotaRefusal(errors.New("budget exhausted"))
	if !IsGroupQuotaMessage(err.Error()) {
		t.Fatalf("群组预算拒绝未被认出: %q", err.Error())
	}
	if !strings.Contains(err.Error(), GroupQuotaMarker) {
		t.Fatalf("拒绝消息里没有标记 %q: %q", GroupQuotaMarker, err.Error())
	}
}

// 供应商自己的额度耗尽文案**不能**被当成群组预算拒绝：两者语义相反，
// 混淆会让切 profile 的逻辑对供应商额度失效。
func TestIsGroupQuotaMessageIgnoresProviderQuotaText(t *testing.T) {
	t.Parallel()
	for _, message := range []string{
		"额度不足",
		"额度已用尽",
		"欠费",
		"余额不足",
		"insufficient_quota",
		"billing_hard_limit_reached",
	} {
		if IsGroupQuotaMessage(message) {
			t.Errorf("供应商额度文案被误认成群组预算拒绝: %q", message)
		}
	}
}

// 拒绝文案必须避开供应商额度 markers 的字面 —— 这是与标记双保险的第二道防线：
// 即使标记被谁剥掉了，文案本身也不会被误判成供应商额度耗尽。
//
// 之所以要这样：供应商额度 markers 里就有「额度不足 / 额度已用尽 / 欠费」这些
// 中文短语，平台沿用同一措辞就会撞车。
func TestGroupQuotaRefusalCopyAvoidsProviderQuotaPhrases(t *testing.T) {
	t.Parallel()
	forbidden := []string{"额度不足", "额度已用尽", "欠费", "余额不足"}
	for _, text := range []string{GroupQuotaRefusalCopy, GroupQuotaRefusal(nil).Error()} {
		for _, phrase := range forbidden {
			if strings.Contains(text, phrase) {
				t.Errorf("群组预算拒绝文案含供应商额度字面 %q: %q", phrase, text)
			}
		}
	}
}

// 带 cause 的拒绝消息必须保留 cause（errors.Is 要能穿透），否则上层无法据此
// 区分「预算用尽」与「别的失败」。
func TestGroupQuotaRefusalKeepsCause(t *testing.T) {
	t.Parallel()
	cause := errors.New("budget exhausted")
	err := GroupQuotaRefusal(cause)
	if !errors.Is(err, cause) {
		t.Fatalf("cause 未被保留: %v", err)
	}
	if !IsGroupQuotaMessage(err.Error()) {
		t.Fatalf("带 cause 的拒绝消息未被认出: %q", err.Error())
	}
}
