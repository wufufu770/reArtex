"use client";

// 双层差异面板（BIZ 合并施工 W-5）：按 severity/status/WSTG/STRIDE 筛选，每条以
// 设计层与行为层对照卡片展示；人工裁决（确认/误报）写审计链。派生成意图的动作在
// 建模 agent 侧（propose_biz_intent 工具），面板只做人工裁决半边。
// 0.14.4（批次三）：suggest_close 建议关差徽章（派生意图产生了 confirmed 高危
// finding）、批量裁决（复选 + 模板理由）、WSTG/STRIDE 前缀筛选、分页。

import * as React from "react";

import { toast } from "sonner";

import { StatusBadge } from "@/components/status-badge";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { api } from "@/lib/api";
import type { BizDiffRow, Company } from "@/lib/types";

const DIFF_TYPE_LABEL: Record<BizDiffRow["diff_type"], string> = {
  missing_validation: "缺失校验",
  extra_permission: "额外权限",
  state_bypass: "状态跳跃",
  trust_violation: "信任链断点",
  unexpected_flow: "意外数据流",
  timing_anomaly: "时序冲突",
  unbounded_consumption: "额度未受限",
  workflow_skip: "孤立/跳步动作",
};

const STATUS_LABEL: Record<BizDiffRow["status"], string> = {
  open: "待处理",
  pushed: "已派生",
  confirmed: "已确认",
  false_positive: "误报",
};

const PAGE_SIZE = 50;

// 裁决理由模板（0.14.4 批次三）：批量裁决的常用理由一键填入，可改。
const REASON_TEMPLATES = [
  "复测确认，漏洞成立",
  "复测通过，接口已修复",
  "该行为属预期设计，非缺陷",
  "无法复现，证据不足",
  "影响范围评估后降级处理",
];

export default function BizDiffsPage() {
  const [companies, setCompanies] = React.useState<Company[]>([]);
  const [companyId, setCompanyId] = React.useState<number>(0);
  const [severity, setSeverity] = React.useState("");
  const [status, setStatus] = React.useState("");
  const [wstg, setWstg] = React.useState("");
  const [stride, setStride] = React.useState("");
  const [diffs, setDiffs] = React.useState<BizDiffRow[]>([]);
  const [total, setTotal] = React.useState(0);
  const [page, setPage] = React.useState(0); // 0 起的页码
  const [loading, setLoading] = React.useState(false);
  const [reasonFor, setReasonFor] = React.useState<{ id: number; action: string } | null>(null);
  const [reason, setReason] = React.useState("");
  const [selected, setSelected] = React.useState<Set<number>>(() => new Set());
  const [batchAction, setBatchAction] = React.useState("confirmed");
  const [batchTemplate, setBatchTemplate] = React.useState(REASON_TEMPLATES[0]);
  const [batchBusy, setBatchBusy] = React.useState(false);

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
      .bizDiffs({
        company_id: companyId,
        status: status || undefined,
        severity: severity || undefined,
        wstg: wstg || undefined,
        stride: stride || undefined,
        offset: page * PAGE_SIZE,
        limit: PAGE_SIZE,
      })
      .then((r) => {
        setDiffs(r.diffs ?? []);
        setTotal(r.total ?? 0);
        setSelected(new Set());
      })
      .catch(() => toast.error("差异清单加载失败"))
      .finally(() => setLoading(false));
  }, [companyId, severity, status, wstg, stride, page]);

  React.useEffect(() => {
    load();
  }, [load]);

  const toggle = (id: number) => {
    setSelected((cur) => {
      const next = new Set(cur);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const decide = async () => {
    if (!reasonFor) return;
    try {
      await api.bizDiffDecision(reasonFor.id, reasonFor.action, reason);
      toast.success(
        `差异 #${reasonFor.id} 已裁决（${STATUS_LABEL[reasonFor.action as BizDiffRow["status"]] ?? reasonFor.action}），动作进审计链`,
      );
      setReasonFor(null);
      setReason("");
      load();
    } catch (err) {
      toast.error(`裁决失败：${String(err)}`);
    }
  };

  const batchDecide = async () => {
    if (selected.size === 0) return;
    setBatchBusy(true);
    try {
      const r = await api.bizDiffBatchDecision([...selected], batchAction, batchTemplate);
      toast.success(
        `批量裁决完成：成功 ${r.applied.length}${r.failed.length ? `，失败 ${r.failed.length}` : ""}（审计链已记）`,
      );
      setSelected(new Set());
      load();
    } catch (e) {
      toast.error(`批量裁决失败：${(e as Error).message}`);
    } finally {
      setBatchBusy(false);
    }
  };

  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));

  return (
    <div className="flex h-full flex-col gap-3 p-4">
      <div className="flex flex-wrap items-center gap-2">
        <select
          className="h-8 rounded-md border bg-background px-2 text-sm"
          value={companyId}
          onChange={(e) => {
            setPage(0);
            setCompanyId(Number(e.target.value));
          }}
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
          onChange={(e) => {
            setPage(0);
            setSeverity(e.target.value);
          }}
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
          onChange={(e) => {
            setPage(0);
            setStatus(e.target.value);
          }}
        >
          <option value="">全部状态</option>
          <option value="open">待处理</option>
          <option value="pushed">已派生</option>
          <option value="confirmed">已确认</option>
          <option value="false_positive">误报</option>
        </select>
        <input
          className="h-8 w-36 rounded-md border bg-background px-2 text-xs"
          placeholder="WSTG 前缀（如 WSTG-BUSL）"
          value={wstg}
          onChange={(e) => {
            setPage(0);
            setWstg(e.target.value);
          }}
        />
        <input
          className="h-8 w-28 rounded-md border bg-background px-2 text-xs"
          placeholder="STRIDE（如 S4）"
          value={stride}
          onChange={(e) => {
            setPage(0);
            setStride(e.target.value);
          }}
        />
        <Button variant="outline" size="sm" className="h-8 text-xs" onClick={load} disabled={loading}>
          {loading ? "加载中…" : "刷新"}
        </Button>
        <span className="ml-auto text-muted-foreground text-xs">
          共 {total} 条 · 第 {page + 1}/{totalPages} 页
        </span>
        <Button
          variant="ghost"
          size="sm"
          className="h-8 text-xs"
          disabled={page === 0}
          onClick={() => setPage((p) => p - 1)}
        >
          上一页
        </Button>
        <Button
          variant="ghost"
          size="sm"
          className="h-8 text-xs"
          disabled={page + 1 >= totalPages}
          onClick={() => setPage((p) => p + 1)}
        >
          下一页
        </Button>
      </div>

      {/* 批量裁决条（0.14.4）：选中 ≥1 条时出现；理由模板一键填入可改 */}
      {selected.size > 0 && (
        <div className="flex flex-wrap items-center gap-2 rounded-lg border p-2">
          <span className="font-medium text-xs">已选 {selected.size} 条</span>
          <select
            className="h-8 rounded-md border bg-background px-2 text-xs"
            value={batchAction}
            onChange={(e) => setBatchAction(e.target.value)}
          >
            <option value="confirmed">批量确认</option>
            <option value="false_positive">批量误报</option>
            <option value="open">批量重开</option>
          </select>
          <select
            className="h-8 rounded-md border bg-background px-2 text-xs"
            value={batchTemplate}
            onChange={(e) => setBatchTemplate(e.target.value)}
          >
            {REASON_TEMPLATES.map((t) => (
              <option key={t} value={t}>
                {t}
              </option>
            ))}
          </select>
          <input
            className="h-8 flex-1 rounded-md border bg-background px-2 text-xs"
            value={batchTemplate}
            onChange={(e) => setBatchTemplate(e.target.value)}
            placeholder="批量裁决理由（必填）"
          />
          <Button size="sm" className="h-8 text-xs" disabled={batchBusy || !batchTemplate.trim()} onClick={batchDecide}>
            应用
          </Button>
          <Button variant="ghost" size="sm" className="h-8 text-xs" onClick={() => setSelected(new Set())}>
            取消选择
          </Button>
        </div>
      )}

      <div className="min-h-0 flex-1 space-y-2 overflow-y-auto">
        {diffs.length === 0 && !loading && (
          <div className="flex h-full items-center justify-center text-muted-foreground text-sm">暂无差异记录</div>
        )}
        {diffs.map((d) => (
          <div key={d.id} className="rounded-lg border p-3">
            <div className="flex flex-wrap items-center gap-2">
              <input
                type="checkbox"
                className="size-3.5"
                checked={selected.has(d.id)}
                onChange={() => toggle(d.id)}
                aria-label={`选择差异 #${d.id}`}
              />
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
              {d.suggest_close && (
                <Badge className="border-amber-500/50 bg-amber-500/10 text-amber-600 text-[10px] dark:text-amber-400">
                  建议关差（已产出 confirmed 高危 finding）
                </Badge>
              )}
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
