"use client";

// 双层差异面板（BIZ 合并施工 W-5）：按 severity/status/WSTG 筛选，每条以设计层
// 与行为层对照卡片展示；人工裁决（确认/误报）写审计链。派生成意图的动作在建模
// agent 侧（propose_biz_intent 工具），面板只做人工裁决半边。

import * as React from "react";

import { toast } from "sonner";

import { StatusBadge } from "@/components/status-badge";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { api } from "@/lib/api";
import type { BizLayerDiff, Company } from "@/lib/types";

const DIFF_TYPE_LABEL: Record<BizLayerDiff["diff_type"], string> = {
  missing_validation: "缺失校验",
  extra_permission: "额外权限",
  state_bypass: "状态跳跃",
  trust_violation: "信任链断点",
  unexpected_flow: "意外数据流",
  timing_anomaly: "时序冲突",
  unbounded_consumption: "额度未受限",
  workflow_skip: "孤立/跳步动作",
};

const STATUS_LABEL: Record<BizLayerDiff["status"], string> = {
  open: "待处理",
  pushed: "已派生",
  confirmed: "已确认",
  false_positive: "误报",
};

export default function BizDiffsPage() {
  const [companies, setCompanies] = React.useState<Company[]>([]);
  const [companyId, setCompanyId] = React.useState<number>(0);
  const [severity, setSeverity] = React.useState("");
  const [status, setStatus] = React.useState("");
  const [diffs, setDiffs] = React.useState<BizLayerDiff[]>([]);
  const [loading, setLoading] = React.useState(false);
  const [reasonFor, setReasonFor] = React.useState<{ id: number; action: string } | null>(null);
  const [reason, setReason] = React.useState("");

  React.useEffect(() => {
    api
      .companies()
      .then((cs) => {
        setCompanies(cs);
        if (cs.length > 0) setCompanyId((prev) => prev || cs[0].id);
      })
      .catch(() => setCompanies([])); // 公司清单读失败：保持空表，页面错误态由 load 呈现
  }, []);

  const load = React.useCallback(() => {
    if (!companyId) return;
    setLoading(true);
    api
      .bizDiffs({ company_id: companyId, status: status || undefined, severity: severity || undefined, limit: 300 })
      .then((r) => setDiffs(r.diffs ?? []))
      .catch(() => toast.error("差异清单加载失败"))
      .finally(() => setLoading(false));
  }, [companyId, severity, status]);

  React.useEffect(() => {
    load();
  }, [load]);

  const decide = async () => {
    if (!reasonFor) return;
    try {
      await api.bizDiffDecision(reasonFor.id, reasonFor.action, reason);
      toast.success(
        `差异 #${reasonFor.id} 已裁决（${STATUS_LABEL[reasonFor.action as BizLayerDiff["status"]] ?? reasonFor.action}），动作进审计链`,
      );
      setReasonFor(null);
      setReason("");
      load();
    } catch (err) {
      toast.error(`裁决失败：${String(err)}`);
    }
  };

  return (
    <div className="flex h-full flex-col gap-3 p-4">
      <div className="flex flex-wrap items-center gap-2">
        <select
          className="h-8 rounded-md border bg-background px-2 text-sm"
          value={companyId}
          onChange={(e) => setCompanyId(Number(e.target.value))}
        >
          {companies.map((c) => (
            <option key={c.id} value={c.id}>
              {c.name}
            </option>
          ))}
        </select>
        <select
          className="h-8 rounded-md border bg-background px-2 text-sm"
          value={severity}
          onChange={(e) => setSeverity(e.target.value)}
        >
          <option value="">全部严重度</option>
          <option value="critical">critical</option>
          <option value="high">high</option>
          <option value="medium">medium</option>
          <option value="low">low</option>
        </select>
        <select
          className="h-8 rounded-md border bg-background px-2 text-sm"
          value={status}
          onChange={(e) => setStatus(e.target.value)}
        >
          <option value="">全部状态</option>
          <option value="open">待处理</option>
          <option value="pushed">已派生</option>
          <option value="confirmed">已确认</option>
          <option value="false_positive">误报</option>
        </select>
        <Button variant="outline" size="sm" className="h-8 text-xs" onClick={load} disabled={loading}>
          {loading ? "加载中…" : "刷新"}
        </Button>
        <span className="ml-auto text-muted-foreground text-xs">{diffs.length} 条差异</span>
      </div>

      <div className="min-h-0 flex-1 space-y-2 overflow-y-auto">
        {diffs.length === 0 && !loading && (
          <div className="flex h-full items-center justify-center text-muted-foreground text-sm">暂无差异记录</div>
        )}
        {diffs.map((d) => (
          <div key={d.id} className="rounded-lg border p-3">
            <div className="flex flex-wrap items-center gap-2">
              <StatusBadge domain="finding" value={d.severity} />
              <span className="font-medium text-sm">{DIFF_TYPE_LABEL[d.diff_type]}</span>
              <Badge variant="outline" className="text-[10px]">
                {d.wstg_ref || "WSTG——"}
              </Badge>
              <Badge variant="outline" className="text-[10px]">
                STRIDE {d.stride_ref || "—"}
              </Badge>
              <Badge variant="outline" className="text-[10px]">
                {STATUS_LABEL[d.status]}
              </Badge>
              {d.task_id > 0 && (
                <Badge variant="outline" className="text-[10px]">
                  任务 #{d.task_id}
                </Badge>
              )}
              <div className="ml-auto flex gap-1">
                <Button
                  variant="outline"
                  size="sm"
                  className="h-7 text-xs"
                  onClick={() => {
                    setReasonFor({ id: d.id, action: "confirmed" });
                    setReason("");
                  }}
                >
                  确认
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  className="h-7 text-xs"
                  onClick={() => {
                    setReasonFor({ id: d.id, action: "false_positive" });
                    setReason("");
                  }}
                >
                  标记误报
                </Button>
              </div>
            </div>
            <p className="mt-2 text-muted-foreground text-sm">{d.description}</p>
            <div className="mt-2 grid gap-2 text-xs md:grid-cols-2">
              <pre className="overflow-auto rounded-md bg-muted p-2">
                {JSON.stringify(d.design_ref, null, 2) || "{}"}
              </pre>
              <pre className="overflow-auto rounded-md bg-muted p-2">
                {JSON.stringify(d.behavior_ref, null, 2) || "{}"}
              </pre>
            </div>
            {reasonFor?.id === d.id && (
              <div className="mt-2 flex gap-2">
                <input
                  className="h-8 flex-1 rounded-md border bg-background px-2 text-xs"
                  placeholder="裁决理由（必填）"
                  value={reason}
                  onChange={(e) => setReason(e.target.value)}
                />
                <Button size="sm" className="h-8 text-xs" onClick={decide} disabled={!reason.trim()}>
                  提交裁决
                </Button>
                <Button variant="ghost" size="sm" className="h-8 text-xs" onClick={() => setReasonFor(null)}>
                  取消
                </Button>
              </div>
            )}
          </div>
        ))}
      </div>
    </div>
  );
}
