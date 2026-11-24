// Package events 实现 v2.1 统一架构优化增补 §0.6「统一事件流与反馈闭环」的
// 事件契约与派发控制面（0.15.64，批次 TI-06）。
//
// 三条铁律（§0.6 原文口径）：
//  1. 事件先落库，再异步派发——生产方只写 domain_events outbox（PostgreSQL
//     单体期方案，不引入 Kafka/NATS），派发由 Dispatcher 轮询驱动；
//  2. 事件处理必须幂等、可重试、可观测——派发是 at-least-once：消费幂等的
//     第一道闸是 domain_event_consumers 台账，第二道闸是 handler 自身幂等；
//     失败走指数退避重试，超限入 dead（可观测字段：attempts/last_error/next_attempt_at）；
//  3. 用领域事件连接能力，而不是模块间互相硬编码调用——本包不 import 任何
//     能力模块，消费方通过 RegisterConsumer 订阅，未接线的类型保持无消费方。
//
// 接线边界：生产方（哪条代码路径发哪类事件）与 server 启动期的 Dispatcher
// 装配归 TI-11 及后续任务；本批交付契约 + outbox 存储（db/events.go）+ 派发器。
package events

import (
	"encoding/json"
	"fmt"
	"time"
)

// EventSchemaV1 是事件契约的当前版本（DomainEvent.SchemaVersion）。
const EventSchemaV1 = 1

// §0.6 事件表（产生方/消费方/作用见方案表格；常量名与事件名一一对应）。
// 未列出的新事件必须先在方案层立项再扩此清单——事件类型是跨能力契约，
// 不允许随手发明。
const (
	EventTaskContextUpdated           = "task.context.updated"           // Task Runtime/Graph → Task Intelligence：刷新任务上下文快照
	EventAssetDiscovered              = "asset.discovered"               // Enrichment/Worker → Main Agent、Edge、BizModel：更新资产上下文
	EventTrafficExchangeRecorded      = "traffic.exchange.recorded"      // Traffic → BizModel、Evidence：从真实请求响应提取行为事实
	EventHypothesisProposed           = "hypothesis.proposed"            // Edge/Main Agent → Main Agent、Validation：进假设队列并评估优先级
	EventBusinessModelUpdated         = "business.model.updated"         // BizModel → Task Intelligence、Main Agent：触发上下文刷新
	EventBusinessModelDeltaDetected   = "business.model.delta_detected"  // BizModel → Main Agent、Edge：生成待验证假设
	EventEvidenceRecorded             = "evidence.recorded"              // Tools/Traffic/Validation → Hypothesis、Finding、BizModel：支持或反驳假设
	EventFindingCreated               = "finding.created"                // Finding Pipeline → Main Agent、Reporting、Knowledge
	EventFindingUpdated               = "finding.updated"                // Finding Pipeline → Main Agent、Reporting、Knowledge
	EventCapabilityExecutionCompleted = "capability.execution.completed" // Capability Runtime → Task Intelligence、Main Agent：汇总输出并决定下一步
	EventTaskGoalEvaluated            = "task.goal_evaluated"            // Goal Gate → Main Agent、Learning：统一任务收口
	EventHarnessRegressionDetected    = "harness.regression.detected"    // Eval/CRSI → SIM/CRSI：仅用于 Harness 改进诊断
)

// DomainEvent 是 §0.6 事件的统一 envelope（字段与方案 Go 骨架一字对应）。
//
// 字段映射说明（方案骨架 ↔ 本仓库现状）：
//   - TenantID 是方案的租户维度字符串；本仓库的租户维度是 company_id（bigint），
//     库层投影见 db/domain_events.company_id，空串 = 无企业维度；
//   - ScopeVersion 是方案的范围快照字符串；本仓库的范围水位是 task_scope
//     MAX(id) 水位（bigint，见 db.ScopeVersionForTask），空串 = 未记录。
type DomainEvent struct {
	ID            string          `json:"id"`
	Type          string          `json:"type"`
	SchemaVersion int             `json:"schema_version"`
	TaskID        int64           `json:"task_id"`
	TenantID      string          `json:"tenant_id"`
	ExecutionID   string          `json:"execution_id"`
	CorrelationID string          `json:"correlation_id"`
	CausationID   string          `json:"causation_id"`
	ScopeVersion  string          `json:"scope_version"`
	OccurredAt    time.Time       `json:"occurred_at"`
	Payload       json.RawMessage `json:"payload"`
}

// ValidateEvent 校验事件 envelope：ID/Type 必填，SchemaVersion 必须等于当前
// 契约版本（版本不符在入口拒绝——事件是跨能力契约，带病版本不允许入库）。
func ValidateEvent(ev DomainEvent) error {
	if ev.ID == "" {
		return errEvent("事件缺 ID（uuid 幂等键）")
	}
	if ev.Type == "" {
		return errEvent("事件缺 Type")
	}
	if ev.SchemaVersion != EventSchemaV1 {
		return errEventf("事件 schema_version %d 不是当前契约 v%d——请升级生产方", ev.SchemaVersion, EventSchemaV1)
	}
	if ev.Payload == nil {
		return errEvent("事件 payload 为必填（无载荷请显式传 {}）")
	}
	return nil
}

type errEvent string

func (e errEvent) Error() string { return string(e) }

func errEventf(format string, args ...any) error {
	return errEvent(fmt.Sprintf(format, args...))
}
