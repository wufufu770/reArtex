"use client";

import * as React from "react";

import Link from "next/link";

import { CircleCheckIcon, CircleXIcon, ScrollTextIcon } from "lucide-react";
import { toast } from "sonner";

import { StatusBadge } from "@/components/status-badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { api } from "@/lib/api";
import type { StrategyProposal } from "@/lib/types";

import { DecisionDialog } from "../_components/decision-dialog";
import { fmtDateTime, fmtJSON, PROPOSAL_STATUSES } from "../_components/strategy-format";
import { StrategyNav } from "../_components/strategy-nav";

// ProposalList 是提议清单的渲染：加载中 / 空态 / 表格三种。
function ProposalList({
  rows,
  loaded,
  onDecide,
}: {
  rows: StrategyProposal[];
  loaded: boolean;
  onDecide: (id: number, status: "approved" | "rejected") => void;
}) {
  if (!loaded) {
    return <p className="py-6 text-center text-muted-foreground text-sm">加载中…</p>;
  }
  if (rows.length === 0) {
    return (
      <Empty className="mx-4 border border-dashed lg:mx-6">
        <EmptyHeader>
          <EmptyTitle>没有待裁决的提议</EmptyTitle>
          <EmptyDescription>
            提议由 agent 在跑任务时提出（改现有策略 / 新建策略 / 退役策略）。 当前没有等着批的 ——
            切到别的状态可以看历史裁决。
          </EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  }
  return (
    <Table className="**:data-[slot='table-cell']:px-4 **:data-[slot='table-head']:px-4">
      <TableHeader className="[&_tr]:border-t">
        <TableRow>
          <TableHead>提议</TableHead>
          <TableHead>关联策略</TableHead>
          <TableHead className="hidden lg:table-cell">提议内容</TableHead>
          <TableHead>状态</TableHead>
          <TableHead>操作</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((p) => (
          <TableRow key={p.id}>
            <TableCell>
              <div className="flex flex-col items-start gap-1">
                <span className="font-mono text-xs">#{p.id}</span>
                <StatusBadge domain="proposalRoute" value={p.route} />
                <span className="text-muted-foreground text-xs">
                  {p.created_by || "—"} · {fmtDateTime(p.created_at)}
                </span>
              </div>
            </TableCell>
            <TableCell>
              {p.strategy_id > 0 ? (
                <Link href={`/strategies/detail?id=${p.strategy_id}`} className="font-mono text-xs hover:text-primary">
                  #{p.strategy_id}
                </Link>
              ) : (
                <span className="text-muted-foreground text-xs">未关联</span>
              )}
            </TableCell>
            <TableCell className="hidden lg:table-cell">
              <div className="flex max-w-md flex-col gap-1">
                <span className="text-xs">{p.reason || "（提议未写理由）"}</span>
                <pre className="max-h-24 overflow-auto whitespace-pre-wrap rounded-md bg-muted/50 p-2 font-mono text-[10px]">
                  {fmtJSON(p.payload)}
                </pre>
              </div>
            </TableCell>
            <TableCell>
              <div className="flex flex-col items-start gap-1">
                <StatusBadge domain="proposal" value={p.status} dot />
                {p.status !== "pending" && (
                  <span className="text-muted-foreground text-xs">
                    {p.decided_by || "—"} · {fmtDateTime(p.decided_at)}
                  </span>
                )}
                {p.status !== "pending" && p.decided_reason && (
                  <span className="max-w-48 text-muted-foreground text-xs">{p.decided_reason}</span>
                )}
              </div>
            </TableCell>
            <TableCell>
              {p.status === "pending" ? (
                <div className="flex items-center gap-2">
                  <Button size="sm" onClick={() => onDecide(p.id, "approved")}>
                    <CircleCheckIcon /> 批准
                  </Button>
                  <Button size="sm" variant="outline" onClick={() => onDecide(p.id, "rejected")}>
                    <CircleXIcon /> 驳回
                  </Button>
                </div>
              ) : (
                <span className="text-muted-foreground text-xs">已裁决</span>
              )}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

export default function StrategyProposalsPage() {
  const [status, setStatus] = React.useState<string>("pending");
  const [rows, setRows] = React.useState<StrategyProposal[]>([]);
  const [loaded, setLoaded] = React.useState(false);
  // 待裁决的那一条：决定弹窗当前是在批还是在驳。
  const [decision, setDecision] = React.useState<{
    id: number;
    status: "approved" | "rejected";
  } | null>(null);

  const lastRef = React.useRef<string>("");
  const reportedRef = React.useRef(false);

  const load = React.useCallback(async () => {
    try {
      const list = await api.strategyProposals(status);
      const sig = JSON.stringify(list);
      if (sig === lastRef.current) return;
      lastRef.current = sig;
      setRows(list);
      reportedRef.current = false;
    } catch (e) {
      if (!reportedRef.current) {
        reportedRef.current = true;
        toast.error(`读取策略提议失败：${(e as Error).message}`);
      }
    } finally {
      setLoaded(true);
    }
  }, [status]);

  React.useEffect(() => {
    void load();
  }, [load]);

  async function confirm(reason: string, actor: string) {
    if (!decision) return;
    await api.decideStrategyProposal(decision.id, { status: decision.status, reason, actor });
    toast.success(decision.status === "approved" ? "已批准该提议" : "已驳回该提议");
  }

  const pendingCount = rows.filter((p) => p.status === "pending").length;

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div>
        <h1 className="font-semibold text-xl tracking-tight">待裁决提议</h1>
        <p className="text-muted-foreground text-sm">
          agent 提议、宿主裁决 · 批准 ≠ 生效：生效还要过 curator 的三条件（重放闸门 + 观测数 + 里程碑命中）
        </p>
      </div>

      <StrategyNav current="proposals" />

      <Card>
        <CardHeader>
          <CardTitle>提议清单</CardTitle>
          <CardDescription>
            {status === "pending" ? `待裁决 ${pendingCount} 条` : `共 ${rows.length} 条`} · 裁决会带理由与操作人进
            audit_events 哈希链
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          <div className="flex flex-wrap items-center gap-2">
            <Select
              value={status}
              onValueChange={(v) => {
                lastRef.current = "";
                setStatus(v);
              }}
            >
              <SelectTrigger className="w-full sm:w-56">
                <SelectValue placeholder="状态" />
              </SelectTrigger>
              <SelectContent>
                {PROPOSAL_STATUSES.map((s) => (
                  <SelectItem key={s.value} value={s.value}>
                    {s.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <ProposalList rows={rows} loaded={loaded} onDecide={(id, st) => setDecision({ id, status: st })} />
        </CardContent>
      </Card>

      <p className="text-muted-foreground text-xs">
        <ScrollTextIcon className="mr-1 inline size-3.5 align-text-bottom" />
        裁决写只发生在宿主侧：判据是状态比 + 必填字段（理由 + 操作人），模型不参与。
      </p>

      <DecisionDialog
        open={decision !== null}
        onOpenChange={(next) => {
          if (!next) setDecision(null);
        }}
        title={decision?.status === "approved" ? "批准提议" : "驳回提议"}
        description={
          decision?.status === "approved"
            ? "批准只代表「人批了这条提议」，策略本身不会因此生效 —— 生效还要过 curator 的三条件。"
            : "驳回同样要留理由与操作人：事后要能查清是谁、为什么驳的。"
        }
        confirmLabel={decision?.status === "approved" ? "批准" : "驳回"}
        onConfirm={confirm}
        onDone={() => {
          lastRef.current = "";
          void load();
        }}
      />
    </div>
  );
}
