"use client";

import * as React from "react";

import Link from "next/link";

import { ScrollTextIcon } from "lucide-react";
import { toast } from "sonner";

import { StatusBadge } from "@/components/status-badge";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { api } from "@/lib/api";
import type { SodDecision, SodDecisionLedger, Task } from "@/lib/types";

import { ChainStatusCard } from "./_components/chain-status-card";
import { fmtDateTime, fmtDigest, fmtLatency, SOD_LIMIT_OPTIONS, sodScopeLabel } from "./_components/sod-format";
import { SodNav } from "./_components/sod-nav";

// ALL_TASKS 表示「不按任务筛」—— 后端不传 task_id 就是跨任务看账。
const ALL_TASKS = "__all__";

// 账本在任务跑动时一直增长，10 秒一轮足够跟上，又不至于把库读爆。
const POLL_MS = 10_000;

// DecisionRow 是账本一行的渲染。
//
// 分层（tier）是**读时推导**的（下界，不是真值），所以这一列只呈现后端给的
// 结论，不在这里重新解释「所以其实更严」—— 那是后端 tier_note 的口径。
function DecisionRow({ row }: { row: SodDecision }) {
  return (
    <TableRow>
      <TableCell>
        <div className="flex flex-col items-start gap-0.5">
          <span className="text-xs tabular-nums">{fmtDateTime(row.created_at)}</span>
          <span className="font-mono text-muted-foreground text-xs">#{row.id}</span>
        </div>
      </TableCell>
      <TableCell>
        <div className="flex flex-col items-start gap-0.5">
          <span className="font-mono text-xs">{row.tool_name || "—"}</span>
          <span className="text-muted-foreground text-xs">{row.source_agent || "—"}</span>
          {row.task_id !== undefined && (
            <Link
              href={`/function/tasks/detail?id=${row.task_id}`}
              className="font-mono text-muted-foreground text-xs hover:text-primary"
            >
              任务 {row.task_id}
            </Link>
          )}
        </div>
      </TableCell>
      <TableCell>
        <div className="flex flex-col items-start gap-1">
          <StatusBadge domain="sodVerdict" value={row.verdict} dot />
          <StatusBadge domain="sodReason" value={row.reason_code} />
          {row.anomaly_flag && (
            <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
              异常标记{row.anomaly_reason ? ` · ${row.anomaly_reason}` : ""}
            </Badge>
          )}
        </div>
      </TableCell>
      <TableCell>
        <div className="flex flex-col items-start gap-1">
          <StatusBadge domain="sodTier" value={row.tier} />
          <StatusBadge domain="sodTierCategory" value={row.tier_category} />
          {row.tier_host_only && (
            <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
              仅宿主侧写面
            </Badge>
          )}
          <span className="text-muted-foreground text-xs">{sodScopeLabel(row.tier_scope_in)}</span>
        </div>
      </TableCell>
      <TableCell className="hidden lg:table-cell">
        <div className="flex max-w-md flex-col gap-1">
          <span className="font-mono text-muted-foreground text-xs">{fmtDigest(row.payload_digest)}</span>
          {row.payload_preview && (
            <pre className="max-h-20 overflow-auto whitespace-pre-wrap rounded-md bg-muted/50 p-2 font-mono text-[10px]">
              {row.payload_preview}
            </pre>
          )}
        </div>
      </TableCell>
      <TableCell>
        <div className="flex flex-col items-start gap-1">
          {row.cache_hit ? (
            <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
              命中缓存
            </Badge>
          ) : (
            <span className="text-muted-foreground text-xs">未命中缓存</span>
          )}
          <span className="text-muted-foreground text-xs tabular-nums">{fmtLatency(row.latency_ms)}</span>
        </div>
      </TableCell>
    </TableRow>
  );
}

// DecisionList 是账本表的渲染：加载中 / 空态 / 表格三种。
function DecisionList({ rows, loaded }: { rows: SodDecision[]; loaded: boolean }) {
  if (!loaded) {
    return <p className="py-6 text-center text-muted-foreground text-sm">加载中…</p>;
  }
  if (rows.length === 0) {
    return (
      <Empty className="mx-4 border border-dashed lg:mx-6">
        <EmptyHeader>
          <EmptyTitle>账本还是空的</EmptyTitle>
          <EmptyDescription>
            账本记的是每一次工具裁决（放行 / 拒绝 / 待证伪）—— 只有闸门真的判过一次，才会留下一行。
            全新部署还没有任何裁决，所以这里什么都没有；跑起来一个任务后回来再看。
          </EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  }
  return (
    <Table className="**:data-[slot='table-cell']:px-4 **:data-[slot='table-head']:px-4">
      <TableHeader className="[&_tr]:border-t">
        <TableRow>
          <TableHead>时间</TableHead>
          <TableHead>工具 / 来源</TableHead>
          <TableHead>裁决</TableHead>
          <TableHead>分层</TableHead>
          <TableHead className="hidden lg:table-cell">载荷</TableHead>
          <TableHead>缓存 / 延迟</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((row) => (
          <DecisionRow key={row.id} row={row} />
        ))}
      </TableBody>
    </Table>
  );
}

export default function SodLedgerPage() {
  const [taskID, setTaskID] = React.useState<string>(ALL_TASKS);
  const [limit, setLimit] = React.useState<string>(String(SOD_LIMIT_OPTIONS[1]));
  const [tasks, setTasks] = React.useState<Task[]>([]);
  const [ledger, setLedger] = React.useState<SodDecisionLedger | null>(null);
  const [loaded, setLoaded] = React.useState(false);

  // 内容没变就不 setState，避免整页重渲染（与策略库页同一套做法）。
  const lastRef = React.useRef<string>("");
  // 首屏失败要报一次，之后的轮询失败静默（下一轮自动重试）。
  const reportedRef = React.useRef(false);

  const load = React.useCallback(async () => {
    try {
      const taskId = taskID === ALL_TASKS ? undefined : Number(taskID);
      const list = await api.sodDecisions({ taskId, limit: Number(limit) });
      const sig = JSON.stringify(list);
      if (sig === lastRef.current) return;
      lastRef.current = sig;
      setLedger(list);
      reportedRef.current = false;
    } catch (e) {
      if (!reportedRef.current) {
        reportedRef.current = true;
        toast.error(`读取决策账本失败：${(e as Error).message}`);
      }
    } finally {
      setLoaded(true);
    }
  }, [taskID, limit]);

  React.useEffect(() => {
    void load();
    const t = setInterval(() => void load(), POLL_MS);
    return () => clearInterval(t);
  }, [load]);

  // 任务下拉的候选：只取活着的任务（跑完的任务账本不会再来新行）。
  React.useEffect(() => {
    api
      .tasks()
      .then((r) => setTasks(r.tasks))
      .catch(() => setTasks([]));
  }, []);

  const rows = ledger?.decisions ?? [];
  const approveCount = rows.filter((r) => r.verdict === "approve").length;
  const denyCount = rows.filter((r) => r.verdict === "deny").length;
  const reviewCount = rows.filter((r) => r.verdict === "needs_review").length;

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div>
        <h1 className="font-semibold text-xl tracking-tight">职责分离链 · 决策账本</h1>
        <p className="text-muted-foreground text-sm">
          每一次工具裁决的完整留痕 · 放行 / 拒绝 / 待证伪 · 账本是只读的审计记录，没有重判入口
        </p>
      </div>

      <SodNav current="decisions" />


      <ChainStatusCard compact />

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">决策</p>
            <p className="font-semibold text-lg tabular-nums">{rows.length}</p>
          </CardContent>
        </Card>
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">放行</p>
            <p className="font-semibold text-lg tabular-nums">{approveCount}</p>
          </CardContent>
        </Card>
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">拒绝</p>
            <p className="font-semibold text-lg tabular-nums">{denyCount}</p>
          </CardContent>
        </Card>
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">待证伪</p>
            <p className="font-semibold text-lg tabular-nums">{reviewCount}</p>
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>决策账本</CardTitle>
          <CardDescription>
            共 {rows.length} 条{taskID !== ALL_TASKS ? ` · 已按任务 ${taskID} 过滤` : ""} · 单页上限{" "}
            {ledger?.limit ?? limit} 条
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          <div className="flex flex-wrap items-center gap-2">
            <Select
              value={taskID}
              onValueChange={(v) => {
                lastRef.current = "";
                setTaskID(v);
              }}
            >
              <SelectTrigger className="w-full sm:w-64">
                <SelectValue placeholder="任务" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={ALL_TASKS}>全部任务</SelectItem>
                {tasks.map((t) => (
                  <SelectItem key={t.id} value={t.id}>
                    {t.name || t.description || `任务 ${t.id}`}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Select
              value={limit}
              onValueChange={(v) => {
                lastRef.current = "";
                setLimit(v);
              }}
            >
              <SelectTrigger className="w-full sm:w-40">
                <SelectValue placeholder="条数" />
              </SelectTrigger>
              <SelectContent>
                {SOD_LIMIT_OPTIONS.map((n) => (
                  <SelectItem key={n} value={String(n)}>
                    最近 {n} 条
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <DecisionList rows={rows} loaded={loaded} />
        </CardContent>
      </Card>

      {ledger?.tier_note && <p className="text-muted-foreground text-xs">{ledger.tier_note}</p>}

      <p className="text-muted-foreground text-xs">
        <ScrollTextIcon className="mr-1 inline size-3.5 align-text-bottom" />
        账本是审计记录：同一笔裁决只此一条，改不了也不重跑 —— 所以这里没有「重新裁决」这种按钮。 「待证伪」的那几笔要去
        <Link href="/sod/review-queue" className="mx-0.5 underline underline-offset-4">
          对抗复核队列
        </Link>
        看，但那张队列是给 redteam 证伪用的，不是人点头的地方。
      </p>
    </div>
  );
}
