"use client";

import * as React from "react";

import { ShieldAlertIcon } from "lucide-react";
import { toast } from "sonner";

import { EgressGovernanceNotice } from "@/components/egress-governance-notice";
import { StatusBadge } from "@/components/status-badge";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { api } from "@/lib/api";
import type { SodReviewItem, SodReviewQueue } from "@/lib/types";

import { fmtDateTime, fmtDigest, fmtJSON } from "../_components/sod-format";
import { SodNav } from "../_components/sod-nav";

// ReviewList 是复核队列的渲染：加载中 / 空态 / 表格三种。
//
// ⚠️ 这个列表**没有**操作列：这一页不提供「同意 / 拒绝」这种按钮，也不该有。
function ReviewList({ rows, loaded }: { rows: SodReviewItem[]; loaded: boolean }) {
  if (!loaded) {
    return <p className="py-6 text-center text-muted-foreground text-sm">加载中…</p>;
  }
  if (rows.length === 0) {
    return (
      <Empty className="mx-4 border border-dashed lg:mx-6">
        <EmptyHeader>
          <EmptyTitle>队列是空的</EmptyTitle>
          <EmptyDescription>
            队列里只会有 needs_review 的那几笔 —— 闸门证据不足以拒绝、但值得让对抗者证伪的动作才会入队。
            现在没有待证伪的动作。
          </EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  }
  return (
    <Table className="**:data-[slot='table-cell']:px-4 **:data-[slot='table-head']:px-4">
      <TableHeader className="[&_tr]:border-t">
        <TableRow>
          <TableHead>项</TableHead>
          <TableHead>工具 / 来源</TableHead>
          <TableHead>入队原因</TableHead>
          <TableHead className="hidden lg:table-cell">载荷 / 结构化意图</TableHead>
          <TableHead>状态</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((it) => (
          <TableRow key={it.id}>
            <TableCell>
              <div className="flex flex-col items-start gap-0.5">
                <span className="font-mono text-xs">#{it.id}</span>
                <span className="text-muted-foreground text-xs tabular-nums">{fmtDateTime(it.created_at)}</span>
              </div>
            </TableCell>
            <TableCell>
              <div className="flex flex-col items-start gap-0.5">
                <span className="font-mono text-xs">{it.tool_name || "—"}</span>
                <span className="text-muted-foreground text-xs">{it.source_agent || "—"}</span>
                {it.task_id !== undefined && (
                  <span className="font-mono text-muted-foreground text-xs">任务 {it.task_id}</span>
                )}
              </div>
            </TableCell>
            <TableCell>
              <div className="flex flex-col items-start gap-1">
                <StatusBadge domain="sodReason" value={it.reason_code} />
                {it.anomaly_flag && (
                  <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
                    异常标记{it.anomaly_reason ? ` · ${it.anomaly_reason}` : ""}
                  </Badge>
                )}
              </div>
            </TableCell>
            <TableCell className="hidden lg:table-cell">
              <div className="flex max-w-md flex-col gap-1">
                <span className="font-mono text-muted-foreground text-xs">{fmtDigest(it.payload_digest)}</span>
                {it.payload_preview && (
                  <pre className="max-h-16 overflow-auto whitespace-pre-wrap rounded-md bg-muted/50 p-2 font-mono text-[10px]">
                    {it.payload_preview}
                  </pre>
                )}
                <pre className="max-h-16 overflow-auto whitespace-pre-wrap rounded-md bg-muted/50 p-2 font-mono text-[10px]">
                  {fmtJSON(it.structured_intent)}
                </pre>
              </div>
            </TableCell>
            <TableCell>
              <div className="flex flex-col items-start gap-1">
                <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
                  {it.status || "pending"}
                </Badge>
                {it.claimed_by && <span className="text-muted-foreground text-xs">认领人 {it.claimed_by}</span>}
                {it.resolution && <span className="max-w-48 text-muted-foreground text-xs">{it.resolution}</span>}
              </div>
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

export default function SodReviewQueuePage() {
  const [data, setData] = React.useState<SodReviewQueue | null>(null);
  const [loaded, setLoaded] = React.useState(false);

  const lastRef = React.useRef<string>("");
  const reportedRef = React.useRef(false);

  const load = React.useCallback(async () => {
    try {
      const q = await api.sodReviewQueue();
      const sig = JSON.stringify(q);
      if (sig === lastRef.current) return;
      lastRef.current = sig;
      setData(q);
      reportedRef.current = false;
    } catch (e) {
      if (!reportedRef.current) {
        reportedRef.current = true;
        toast.error(`读取对抗复核队列失败：${(e as Error).message}`);
      }
    } finally {
      setLoaded(true);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  const queue = data?.queue ?? [];
  const pendingCount = queue.filter((it) => it.status === "pending").length;
  const claimedCount = queue.filter((it) => it.status === "claimed").length;

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div>
        <h1 className="font-semibold text-xl tracking-tight">对抗复核队列</h1>
        <p className="text-muted-foreground text-sm">
          交给对抗者（redteam）证伪的 flagged 调用 · 本页只读：没有审批动作，也没有任何自动结案
        </p>
      </div>

      <SodNav current="review-queue" />

      <Alert variant="default">
        <ShieldAlertIcon />
        <AlertTitle>这份队列是给对抗者证伪的，不是人工审批队列</AlertTitle>
        <AlertDescription>
          队列里的每一项都是闸门判成 needs_review 的动作：证据不足以直接拒绝，但值得让 redteam
          去证伪。交给提出它的一方去复核等于自证 —— 所以这一页只**读**， 在这里点「同意 /
          拒绝」不构成任何裁决：人工审批走的是另一张表（拦截规则的待审批队列）。
        </AlertDescription>
      </Alert>

      <EgressGovernanceNotice />

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">队列</p>
            <p className="font-semibold text-lg tabular-nums">{queue.length}</p>
          </CardContent>
        </Card>
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">待证伪</p>
            <p className="font-semibold text-lg tabular-nums">{pendingCount}</p>
          </CardContent>
        </Card>
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">已认领</p>
            <p className="font-semibold text-lg tabular-nums">{claimedCount}</p>
          </CardContent>
        </Card>
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">消费者</p>
            <p className="font-semibold text-lg">{data?.consumer || "—"}</p>
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>待证伪清单</CardTitle>
          <CardDescription>
            共 {queue.length} 项 · {data?.note ?? "这一页只读：结案是 redteam 的动作，不在 UI 里"}
          </CardDescription>
        </CardHeader>
        <CardContent>
          <ReviewList rows={queue} loaded={loaded} />
        </CardContent>
      </Card>
    </div>
  );
}
