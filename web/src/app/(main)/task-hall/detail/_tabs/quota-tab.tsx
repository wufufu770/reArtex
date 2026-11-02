"use client";

import * as React from "react";

import { toast } from "sonner";

import { StatusBadge } from "@/components/status-badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { api } from "@/lib/api";
import type { TaskHallQuota } from "@/lib/types";

import { QuotaMeter } from "../../_components/quota-meter";
import { fmtTokens } from "../../_components/task-hall-format";

const QUOTA_STATES = ["active", "draining", "parked"] as const;
type QuotaState = (typeof QUOTA_STATES)[number];

// QuotaTab 是额度的读数与改写。
//
// 改额度/状态走 PUT /api/task-hall/groups/{id}/quota，理由必填（额度与状态都是
// 授权决定，事后要能追责）。后端每次变更都会追加一条审计事件，响应里的 audited
// 字段告诉你这一笔有没有进哈希链。
export function QuotaTab({
  groupId,
  quota,
  onChanged,
}: {
  groupId: number;
  quota: TaskHallQuota | null;
  onChanged: () => void;
}) {
  const [budget, setBudget] = React.useState("");
  const [state, setState] = React.useState<QuotaState>("active");
  const [reason, setReason] = React.useState("");
  const [saving, setSaving] = React.useState(false);

  React.useEffect(() => {
    setBudget(quota ? String(quota.budget_tokens) : "");
    setState((quota?.state as QuotaState) ?? "active");
  }, [quota]);

  const budgetTokens = Number(budget.trim());
  const canSave = reason.trim().length > 0 && Number.isFinite(budgetTokens) && budgetTokens >= 0 && !saving;

  async function save() {
    setSaving(true);
    try {
      const result = await api.setTaskHallQuota(groupId, {
        budget_tokens: budgetTokens,
        state,
        reason: reason.trim(),
      });
      if (!result.audited) {
        toast.error("变更已生效，但这一笔没有进审计哈希链", { duration: 12000 });
      } else {
        toast.success("额度已更新");
      }
      setReason("");
      onChanged();
    } catch (e) {
      toast.error(`更新失败：${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <CardHeader>
          <CardTitle>额度读数</CardTitle>
          <CardDescription>
            {quota
              ? `自然日周期（${quota.period_kind}）· 周期键 ${quota.period_key} · 口径 = 输入 + 输出`
              : "该群还没有预算行 —— 没有额度的群不允许发起模型调用"}
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          {quota ? (
            <>
              <div className="flex items-center gap-2">
                <StatusBadge domain="quota" value={quota.state} dot />
                <span className="text-muted-foreground text-xs">上限 {fmtTokens(quota.budget_tokens)} token</span>
              </div>
              <QuotaMeter
                consumed={quota.consumed_tokens}
                reserved={quota.reserved_tokens}
                budget={quota.budget_tokens}
              />
              <div className="grid grid-cols-3 gap-3 text-sm">
                <div className="flex flex-col gap-0.5">
                  <span className="text-muted-foreground text-xs">消费</span>
                  <span className="font-medium tabular-nums">{fmtTokens(quota.consumed_tokens)}</span>
                </div>
                <div className="flex flex-col gap-0.5">
                  <span className="text-muted-foreground text-xs">预扣</span>
                  <span className="font-medium tabular-nums">{fmtTokens(quota.reserved_tokens)}</span>
                </div>
                <div className="flex flex-col gap-0.5">
                  <span className="text-muted-foreground text-xs">剩余</span>
                  <span className={`font-medium tabular-nums ${quota.remaining_tokens < 0 ? "text-red-600" : ""}`}>
                    {fmtTokens(quota.remaining_tokens)}
                  </span>
                </div>
              </div>
            </>
          ) : (
            <p className="text-muted-foreground text-sm">在下面填一个额度并保存，即可为这个群开额度。</p>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>调整额度</CardTitle>
          <CardDescription>额度与状态都是授权决定，变更必须写明理由，事后可追责</CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4">
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="grid gap-1.5">
              <Label htmlFor="quota-budget">额度（token）</Label>
              <Input
                id="quota-budget"
                type="number"
                min={0}
                value={budget}
                onChange={(e) => setBudget(e.target.value)}
              />
              <p className="text-muted-foreground text-xs">
                0 = 没有可用预算，模型调用一律被拒（没有「不限量」这个取值）。
              </p>
            </div>
            <div className="grid gap-1.5">
              <Label>预算状态</Label>
              <Select value={state} onValueChange={(v) => setState(v as QuotaState)}>
                <SelectTrigger className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="active">active · 正常，允许发起新调用</SelectItem>
                  <SelectItem value="draining">draining · 收尾，不再开新活</SelectItem>
                  <SelectItem value="parked">parked · 冻结，整体停下</SelectItem>
                </SelectContent>
              </Select>
              <p className="text-muted-foreground text-xs">draining 与 parked 都拒绝新的模型调用，在途预扣照常结算。</p>
            </div>
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="quota-reason">变更理由</Label>
            <Input
              id="quota-reason"
              placeholder="如：今日复测预算收紧到 200k"
              value={reason}
              onChange={(e) => setReason(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">理由必填，没有理由的额度变更事后追不回来。</p>
          </div>
          <div>
            <Button onClick={() => void save()} disabled={!canSave}>
              {saving && <Spinner data-icon="inline-start" />}
              {saving ? "保存中" : "保存额度"}
            </Button>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
