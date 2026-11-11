"use client";

import * as React from "react";

import Link from "next/link";

import { ChevronDownIcon, ChevronRightIcon, GavelIcon, MonitorPlayIcon, ScrollTextIcon } from "lucide-react";
import { toast } from "sonner";

import { SodShadowCard } from "@/components/sod-shadow-card";
import { SodSuggestionCard } from "@/components/sod-suggestion-card";
import { StatusBadge } from "@/components/status-badge";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
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

// ChecksDetail 把闸门 checks（不透明 JSON）渲染成键值卡片。
// v0.9.17 用户拍板「后面的详细信息看不到」：详情不再藏在横向滚动外的列里，
// 统一收进行内展开面板。
function ChecksDetail({ checks }: { checks: unknown }) {
  if (checks === null || checks === undefined) {
    return <span className="text-muted-foreground text-xs">（本条没有检查记录）</span>;
  }
  const entries =
    typeof checks === "object" && !Array.isArray(checks) ? Object.entries(checks as Record<string, unknown>) : [];
  if (entries.length === 0) {
    return <pre className="break-all rounded-md bg-muted/50 p-2 font-mono text-[10px]">{String(checks)}</pre>;
  }
  return (
    <div className="grid gap-1 sm:grid-cols-2 xl:grid-cols-3">
      {entries.map(([k, v]) => (
        <div key={k} className="rounded border bg-muted/40 px-2 py-1">
          <div className="text-[10px] font-semibold text-muted-foreground">{k}</div>
          <div className="break-all font-mono text-[10px]">{typeof v === "string" ? v : JSON.stringify(v)}</div>
        </div>
      ))}
    </div>
  );
}

// DecisionDetailPanel 是行内展开的详情面板：一行裁决的**全部**信息——
// 完整载荷、五检查结论、分层理由、异常理由、各摘要。横向滚不出列时看这里。
function DecisionDetailPanel({ row }: { row: SodDecision }) {
  return (
    <div className="grid gap-3 border-b bg-muted/20 px-4 py-3">
      <div className="grid gap-1">
        <p className="font-semibold text-[11px] text-muted-foreground">检查结论（checks）</p>
        <ChecksDetail checks={row.checks} />
      </div>
      <div className="grid gap-1">
        <p className="font-semibold text-[11px] text-muted-foreground">载荷预览（原文不入库，此处即全部留痕）</p>
        <pre className="max-h-60 overflow-y-auto rounded-md border bg-background p-2 font-mono text-[10px] whitespace-pre-wrap break-all">
          {row.payload_preview || "（空）"}
        </pre>
      </div>
      <div className="grid gap-2 md:grid-cols-2">
        <div className="grid content-start gap-1">
          <p className="font-semibold text-[11px] text-muted-foreground">分层推导</p>
          <p className="text-xs">{row.tier_reason || "（无分层理由）"}</p>
          <p className="text-muted-foreground text-[10px]">范围结论：{sodScopeLabel(row.tier_scope_in)}</p>
        </div>
        <div className="grid content-start gap-1">
          <p className="font-semibold text-[11px] text-muted-foreground">摘要与异常</p>
          <p className="break-all font-mono text-[10px] text-muted-foreground">
            payload {fmtDigest(row.payload_digest)}
          </p>
          <p className="break-all font-mono text-[10px] text-muted-foreground">intent {fmtDigest(row.intent_digest)}</p>
          <p className="break-all font-mono text-[10px] text-muted-foreground">token {fmtDigest(row.token_digest)}</p>
          <p className="break-all font-mono text-[10px] text-muted-foreground">scope {fmtDigest(row.scope_digest)}</p>
          {row.anomaly_flag && <p className="text-[10px]">异常标记：{row.anomaly_reason || "（无理由）"}</p>}
        </div>
      </div>
    </div>
  );
}

// DecisionRow 是账本一行的渲染。
//
// v0.10.1 用户拍板「太宽了，后面的都看不到」：全部徽章内联（横排可折行），
// 载荷列单行截断（全文在行内详情面板），内边距收紧——整表收进一屏，不再
// 横向裁切。行高从 ~180px 压到两行以内。
//
// 分层（tier）是**读时推导**的（下界，不是真值），所以这一列只呈现后端给的
// 结论，不在这里重新解释「所以其实更严」—— 那是后端 tier_note 的口径。
function DecisionRow({
  row,
  onReview,
  detailExpanded,
  onToggleDetail,
}: {
  row: SodDecision;
  onReview?: (row: SodDecision) => void;
  detailExpanded: boolean;
  onToggleDetail: (id: number) => void;
}) {
  return (
    <>
      <TableRow>
        <TableCell className="whitespace-nowrap">
          <div className="flex items-center gap-1">
            <button
              type="button"
              aria-label={detailExpanded ? "收起详情" : "展开详情"}
              title={detailExpanded ? "收起详情" : "展开详情（检查结论/完整载荷/分层理由）"}
              className="shrink-0 text-muted-foreground hover:text-foreground"
              onClick={() => onToggleDetail(row.id)}
            >
              {detailExpanded ? <ChevronDownIcon className="size-3.5" /> : <ChevronRightIcon className="size-3.5" />}
            </button>
            <span className="text-xs tabular-nums">{fmtDateTime(row.created_at)}</span>
            <span className="font-mono text-muted-foreground text-xs">#{row.id}</span>
          </div>
        </TableCell>
        <TableCell>
          <div className="flex max-w-[10rem] flex-wrap items-center gap-x-1.5 gap-y-0.5">
            <span className="font-mono text-xs">{row.tool_name || "—"}</span>
            <span className="text-muted-foreground text-xs">{row.source_agent || "—"}</span>
            {row.task_id !== undefined && (
              <Link
                href={`/function/tasks/detail?id=${row.task_id}`}
                className="font-mono text-muted-foreground text-xs hover:text-primary"
              >
                任务{row.task_id}
              </Link>
            )}
          </div>
        </TableCell>
        <TableCell>
          <div className="flex max-w-[14rem] flex-wrap items-center gap-1">
            <StatusBadge domain="sodVerdict" value={row.verdict} dot />
            <StatusBadge domain="sodReason" value={row.reason_code} />
            {row.anomaly_flag && (
              <Badge variant="outline" className="px-1.5 py-0 text-[10px]" title={row.anomaly_reason}>
                异常
              </Badge>
            )}
          </div>
        </TableCell>
        <TableCell>
          <div className="flex max-w-[16rem] flex-wrap items-center gap-1">
            <StatusBadge domain="sodTier" value={row.tier} />
            <StatusBadge domain="sodTierCategory" value={row.tier_category} />
            {row.tier_host_only && (
              <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
                仅宿主
              </Badge>
            )}
            <span className="text-muted-foreground text-[10px]">{sodScopeLabel(row.tier_scope_in)}</span>
          </div>
        </TableCell>
        <TableCell className="hidden xl:table-cell">
          {/* 单行截断：列里只求「认得出是哪次调用」，全文/五检查在详情面板 */}
          <span
            className="block max-w-[14rem] truncate font-mono text-[10px] text-muted-foreground"
            title={row.payload_preview}
          >
            {row.payload_preview || "—"}
          </span>
        </TableCell>
        <TableCell className="whitespace-nowrap">
          <div className="flex flex-wrap items-center gap-x-1.5 gap-y-0.5">
            {row.cache_hit && (
              <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
                缓存
              </Badge>
            )}
            <span className="text-muted-foreground text-xs tabular-nums">{fmtLatency(row.latency_ms)}</span>
          </div>
        </TableCell>
        <TableCell>
          {(row.verdict === "deny" || row.verdict === "needs_review") && onReview && (
            <Button size="sm" variant="outline" className="h-7 px-2 text-xs" onClick={() => onReview(row)}>
              <GavelIcon /> 复核
            </Button>
          )}
        </TableCell>
      </TableRow>
      {/* v0.9.17：行内详情面板——「后面的详细信息」不再依赖横向滚动 */}
      {detailExpanded && (
        <TableRow className="hover:bg-transparent">
          <TableCell colSpan={7} className="p-0">
            <DecisionDetailPanel row={row} />
          </TableCell>
        </TableRow>
      )}
    </>
  );
}

// DecisionList 是账本表的渲染：加载中 / 空态 / 表格三种。
function DecisionList({
  rows,
  loaded,
  onReview,
  expandedDetails,
  onToggleDetail,
}: {
  rows: SodDecision[];
  loaded: boolean;
  onReview?: (row: SodDecision) => void;
  expandedDetails: Set<number>;
  onToggleDetail: (id: number) => void;
}) {
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
    <div className="max-h-[65svh] overflow-auto rounded-md border">
      {/* v0.10.1 压缩口径：内边距 px-2、徽章内联——整表收进一屏，不横向裁切 */}
      <Table className="**:data-[slot='table-cell']:px-2 **:data-[slot='table-head']:px-2 sm:**:data-[slot='table-cell']:px-3 sm:**:data-[slot='table-head']:px-3">
        <TableHeader className="sticky top-0 z-10 bg-card [&_tr]:border-t">
          <TableRow>
            <TableHead>时间</TableHead>
            <TableHead>工具 / 来源</TableHead>
            <TableHead>裁决</TableHead>
            <TableHead>分层</TableHead>
            <TableHead className="hidden xl:table-cell">载荷</TableHead>
            <TableHead>缓存 / 延迟</TableHead>
            <TableHead>操作</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((row) => (
            <DecisionRow
              key={row.id}
              row={row}
              onReview={onReview}
              detailExpanded={expandedDetails.has(row.id)}
              onToggleDetail={onToggleDetail}
            />
          ))}
        </TableBody>
      </Table>
    </div>
  );
}

// F3：人工复核弹窗的状态与提交。
interface ReviewDraft {
  decision: SodDecision;
  scope: "once" | "task";
  reason: string;
  operator: string;
}

export default function SodLedgerPage() {
  const [taskID, setTaskID] = React.useState<string>(ALL_TASKS);
  const [limit, setLimit] = React.useState<string>(String(SOD_LIMIT_OPTIONS[1]));
  const [tasks, setTasks] = React.useState<Task[]>([]);
  const [ledger, setLedger] = React.useState<SodDecisionLedger | null>(null);
  const [loaded, setLoaded] = React.useState(false);
  const [review, setReview] = React.useState<ReviewDraft | null>(null);
  const [reviewing, setReviewing] = React.useState(false);
  // v0.10.1 行内详情面板的展开集合：检查结论/完整载荷/分层理由收在面板里，
  // 列表行保持压缩（载荷列单行截断），不再依赖表格横向滚动。
  const [expandedDetails, setExpandedDetails] = React.useState<Set<number>>(() => new Set());

  const toggleDetail = React.useCallback((id: number) => {
    setExpandedDetails((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

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
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="font-semibold text-xl tracking-tight">职责分离链 · 决策账本</h1>
          <p className="text-muted-foreground text-sm">
            每一次工具裁决的完整留痕 · 放行 / 拒绝 / 待证伪 · 账本是只读的审计记录，没有重判入口
          </p>
        </div>
        {/* v6 主题 A：实时裁决大屏（三列，可挂副屏）的入口 */}
        <Button asChild variant="outline">
          <Link href="/sod/screen">
            <MonitorPlayIcon /> 裁决大屏
          </Link>
        </Button>
      </div>

      <SodNav current="decisions" />

      <ChainStatusCard compact />

      {/* v4.1 合并闸观测面：auditor 建议队列（人工裁决沉淀规则）+ 影子对照（切 enforce 前核对）。 */}
      <SodSuggestionCard />
      <SodShadowCard />

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
          <DecisionList
            rows={rows}
            loaded={loaded}
            onReview={(row) => setReview({ decision: row, scope: "once", reason: "", operator: "sir" })}
            expandedDetails={expandedDetails}
            onToggleDetail={toggleDetail}
          />
        </CardContent>
      </Card>

      {/* E1-1 人工复核改判弹窗：once=用后即焚（默认）/ task=随任务终态过期。
          以人为本（方案 v10 §9）：Tier3 也可人工放行（后端已放开），弹窗对高危
          工具给出醒目警示；基线类（删除/破坏/平台凭据）后端仍 409 硬拦。 */}
      <Dialog open={review !== null} onOpenChange={(open) => !open && setReview(null)}>
        <DialogContent className="max-w-md">
          <DialogHeader>
            <DialogTitle>人工复核改判</DialogTitle>
            <DialogDescription>
              对裁决 #{review?.decision.id}（{review?.decision.tool_name} · {review?.decision.verdict}）发放
              call-precision 放行凭据：只有载荷完全一致的这次调用会被放行，改一个字节就不命中。
            </DialogDescription>
          </DialogHeader>
          {review?.decision.tier === "Tier3" && (
            <p className="rounded-md border border-amber-500/40 bg-amber-500/10 px-3 py-2 text-xs text-amber-600 dark:text-amber-400">
              ⚠ 高危工具人工放行，责任留痕：本次放行将记入审计链（actor/operator
              全程可溯）；删除/破坏/平台凭据类仍被基线硬拦，无法改判。
            </p>
          )}
          {review && (
            <div className="grid gap-3">
              <div className="grid gap-1.5">
                <Label htmlFor="rv-scope">放行范围</Label>
                <Select
                  value={review.scope}
                  onValueChange={(v) => setReview({ ...review, scope: v as "once" | "task" })}
                >
                  <SelectTrigger id="rv-scope">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="once">单次（用后即焚，推荐）</SelectItem>
                    <SelectItem value="task">本任务内（随任务终态过期）</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="grid gap-1.5">
                <Label htmlFor="rv-reason">改判理由（审计留痕）</Label>
                <Input
                  id="rv-reason"
                  value={review.reason}
                  onChange={(e) => setReview({ ...review, reason: e.target.value })}
                  placeholder="例：规整器故障导致的误拒，动作本身合法"
                />
              </div>
              <div className="grid gap-1.5">
                <Label htmlFor="rv-operator">操作者</Label>
                <Input
                  id="rv-operator"
                  value={review.operator}
                  onChange={(e) => setReview({ ...review, operator: e.target.value })}
                />
              </div>
            </div>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setReview(null)}>
              取消
            </Button>
            <Button
              disabled={reviewing || !review?.operator.trim()}
              onClick={() => {
                if (!review) return;
                setReviewing(true);
                api
                  .sodDecisionReview(review.decision.id, {
                    scope: review.scope,
                    reason: review.reason.trim(),
                    operator: review.operator.trim(),
                  })
                  .then((r) => {
                    if (r.suggest_trust_rule) {
                      toast.info(
                        `该工具已被改判 ${r.override_count_same_tool} 次——建议把它沉淀为信任规则（拦截规则页）`,
                      );
                    } else {
                      toast.success("已登记放行凭据：worker 重试同一调用将直接放行");
                    }
                    setReview(null);
                    lastRef.current = "";
                    void load();
                  })
                  .catch((e) => toast.error(`改判失败：${(e as Error).message}`))
                  .finally(() => setReviewing(false));
              }}
            >
              确认放行
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {ledger?.tier_note && <p className="text-muted-foreground text-xs">{ledger.tier_note}</p>}

      <p className="text-muted-foreground text-xs">
        <ScrollTextIcon className="mr-1 inline size-3.5 align-text-bottom" />
        账本是审计记录：同一笔裁决只此一条，本身不可改写；但 deny / 待证伪的裁决可以走「人工复核」发放 call-precision
        放行凭据（改判历史落 sod_manual_overrides，Tier3 高危不可改判）。「待证伪」的那几笔要去
        <Link href="/sod/review-queue" className="mx-0.5 underline underline-offset-4">
          对抗复核队列
        </Link>
        看，但那张队列是给 redteam 证伪用的，不是人点头的地方。
      </p>
    </div>
  );
}
