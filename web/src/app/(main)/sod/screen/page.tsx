"use client";

// 职责分离实时裁决大屏（方案 v6 主题 A）。数据来自 GET /api/sod/feed（2s 轮询聚合），
// 决策动作复用既有 POST /api/intercept/pending/{id}/decide——大屏不新开权限面。
//
// 三列布局：左 = 实时裁决流（sod_decisions 尾随）· 中 = 待人工裁决卡（核心交互区）·
// 右 = 统计与上下文。挂在 (main) 布局里做成页内全屏高度容器（data-content-padding=false
// + h-[calc(100svh-3rem)]），侧栏保留但不再占内容宽度，供副屏/挂墙使用。

import * as React from "react";

import Link from "next/link";

import {
  ActivityIcon,
  ArrowLeftIcon,
  ChevronDownIcon,
  CircleCheckIcon,
  CircleXIcon,
  GavelIcon,
  MonitorPlayIcon,
  PieChartIcon,
  ShieldAlertIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { api } from "@/lib/api";
import type { SodFeed, SodFeedPending, SodFeedRow, SodFeedStats, SodMergedGateMode } from "@/lib/types";
import { cn } from "@/lib/utils";

// 账本在任务跑动时持续增长，2s 一轮对账足够实时；页面不可见时暂停（visibilitychange）。
const POLL_MS = 2_000;
// 后端 recent 本来就 LIMIT 50，这里再兜一道，防止上游口径变化把大屏撑爆。
const MAX_RECENT = 50;

// MODE_META 是合并闸三态的徽标文案与配色（与影子对照卡同一口径）。
const MODE_META: Record<SodMergedGateMode, { label: string; badge: string }> = {
  off: { label: "off · 旧链裁决", badge: "border-border bg-muted text-muted-foreground" },
  shadow: {
    label: "shadow · 影子对照中",
    badge: "border-amber-500/20 bg-amber-500/15 text-amber-600 dark:text-amber-400",
  },
  enforce: {
    label: "enforce · 新链已接管",
    badge: "border-emerald-500/20 bg-emerald-500/15 text-emerald-600 dark:text-emerald-400",
  },
};

// VerdictLevel 是大屏行的裁决分级：决定徽标颜色与整行的淡出/高亮。
type VerdictLevel = "approve" | "auditor_pass" | "deny" | "hard_deny" | "ask" | "needs_review";

// hard_deny 的原因码（后端 platform_baseline / destructive_action / category_deny）。
const HARD_DENY_RE = /destructive|baseline|category_deny/;

// classifyRow 把一行 feed 归到裁决分级。auditor pass 的行（利于挖掘放行）单独分级，
// 渲染成淡色小号快速滚过；deny / 异常标记的行红左边框高亮停留。
function classifyRow(row: SodFeedRow): VerdictLevel {
  if (HARD_DENY_RE.test(row.reason_code)) return "hard_deny";
  if (row.verdict === "deny") return "deny";
  if (row.verdict === "ask" || row.verdict === "needs_review" || row.reason_code.includes("ask")) {
    return row.verdict === "needs_review" ? "needs_review" : "ask";
  }
  if (row.auditor === "pass") return "auditor_pass";
  return "approve";
}

// VERDICT_META 是裁决徽标：approve=绿 / auditor pass=翠绿 / deny=红 / hard_deny=深红 / ask 类=琥珀。
const VERDICT_META: Record<VerdictLevel, { label: string; badge: string }> = {
  approve: { label: "放行", badge: "border-emerald-500/20 bg-emerald-500/15 text-emerald-600 dark:text-emerald-400" },
  auditor_pass: {
    label: "auditor 放行",
    badge: "border-emerald-500/30 bg-emerald-500/20 text-emerald-600 dark:text-emerald-400",
  },
  deny: { label: "拒绝", badge: "border-red-500/20 bg-red-500/15 text-red-600 dark:text-red-400" },
  hard_deny: { label: "硬拦", badge: "border-rose-600 bg-rose-600 text-white" },
  ask: { label: "转人工", badge: "border-amber-500/20 bg-amber-500/15 text-amber-600 dark:text-amber-400" },
  needs_review: { label: "待证伪", badge: "border-amber-500/20 bg-amber-500/15 text-amber-600 dark:text-amber-400" },
};

// FeedRow 是左列裁决流的一行：时间 + agent + 工具 + verdict 徽标 + category 小徽标 + note 一行。
function FeedRow({ row }: { row: SodFeedRow }) {
  const level = classifyRow(row);
  const meta = VERDICT_META[level];
  // auditor pass 的行默认淡色小号快速滚过（方案 §1：不让吞吐噪音淹没异常）。
  const muted = level === "auditor_pass";
  // deny / 异常标记的行红左边框高亮，停留易扫。
  const alert = level === "deny" || level === "hard_deny" || row.reason_code === "anomaly_flagged";

  return (
    <div
      className={cn(
        "rounded-lg border px-2 py-1",
        alert ? "border-red-500/30 border-l-2 border-l-red-500 bg-red-500/5" : "border-border/60 bg-background/40",
        muted && "text-[11px] opacity-60",
      )}
    >
      <div className="flex min-w-0 items-center gap-1.5 text-xs">
        <span className="shrink-0 font-mono text-muted-foreground text-[11px] tabular-nums">{row.ts}</span>
        <span className="truncate font-medium font-mono">{row.agent || "—"}</span>
        <span className="truncate font-mono text-muted-foreground">{row.tool}</span>
        <span
          className={cn(
            "ml-auto shrink-0 whitespace-nowrap rounded-md border px-1.5 py-0.5 font-medium text-[10px]",
            meta.badge,
          )}
        >
          {meta.label}
        </span>
        {/* E1-2：人工改判的放行必须在大屏可见——机器放行与人工改判不能混成一种绿。 */}
        {row.verdict === "approve" && row.overridden && (
          <Badge
            variant="outline"
            className="shrink-0 border-violet-500/30 bg-violet-500/15 px-1 py-0 text-[9px] text-violet-600 dark:text-violet-400"
          >
            改判
          </Badge>
        )}
      </div>
      <div className="mt-0.5 flex min-w-0 items-center gap-1 text-[11px]">
        {row.category && (
          <span className="shrink-0 rounded border border-border/60 bg-muted/40 px-1 py-px text-[10px] text-muted-foreground">
            {row.category}
          </span>
        )}
        {row.relevance && row.relevance !== "none" && (
          <span className="shrink-0 rounded border border-violet-500/20 bg-violet-500/10 px-1 py-px text-[10px] text-violet-600 dark:text-violet-400">
            利用价值 {row.relevance}
          </span>
        )}
        {row.note && <span className="truncate text-muted-foreground">{row.note}</span>}
      </div>
    </div>
  );
}

// PendingCard 是中列的待人工裁决卡：工具 + agent + reason 全文 + 脱敏载荷（可展开）+
// 「同类放行」复选框（E1-5）+ 大按钮。
function PendingCard({
  item,
  busy,
  onDecide,
}: {
  item: SodFeedPending;
  busy: boolean;
  onDecide: (id: number, decision: "allowed" | "denied", taskTool?: boolean) => void;
}) {
  const [expanded, setExpanded] = React.useState(false);
  // E1-5：勾选后允许时一并签发 task_tool 通配凭据（随任务终态过期；基线类永不放行）。
  const [taskTool, setTaskTool] = React.useState(false);
  const hasInput = item.input.trim().length > 0;

  return (
    <Card size="sm" className="gap-2 py-3">
      <CardContent className="grid gap-2">
        <div className="flex min-w-0 items-center gap-2">
          <span className="shrink-0 rounded-md bg-amber-500/15 px-1.5 py-0.5 font-mono font-semibold text-amber-600 text-xs dark:text-amber-400">
            {item.tool || "—"}
          </span>
          <span className="truncate font-mono text-muted-foreground text-xs">{item.agent || "—"}</span>
          <span className="ml-auto shrink-0 font-mono text-[11px] text-muted-foreground tabular-nums">{item.ts}</span>
        </div>

        <p className="break-words text-xs leading-relaxed">{item.reason || "（无理由说明）"}</p>

        {hasInput && (
          <div className="min-w-0">
            <button
              type="button"
              onClick={() => setExpanded((v) => !v)}
              className="inline-flex items-center gap-1 text-[11px] text-muted-foreground transition-colors hover:text-foreground"
            >
              <ChevronDownIcon className={cn("size-3 transition-transform", expanded && "rotate-180")} />
              脱敏载荷（{expanded ? "收起" : "展开"}）
            </button>
            <pre
              className={cn(
                "mt-1 whitespace-pre-wrap break-all rounded-md bg-muted/50 p-2 font-mono text-[10px] leading-relaxed",
                expanded ? "max-h-40 overflow-y-auto" : "line-clamp-3",
              )}
            >
              {item.input}
            </pre>
          </div>
        )}

        <label
          htmlFor={`task-tool-${item.id}`}
          className="flex cursor-pointer items-start gap-2 pt-1 text-muted-foreground text-xs"
        >
          <Checkbox
            id={`task-tool-${item.id}`}
            checked={taskTool}
            onCheckedChange={(v) => setTaskTool(v === true)}
            disabled={busy}
            className="mt-0.5"
          />
          <span className="flex flex-col gap-0.5">
            <span>同时放行本任务后续同类调用</span>
            <span className="text-[11px]">签发 task_tool 凭据，随任务终态过期；基线类永不放行</span>
          </span>
        </label>

        <div className="grid grid-cols-2 gap-2 pt-1">
          <Button
            variant="outline"
            disabled={busy}
            className="border-emerald-500/40 text-emerald-600 hover:bg-emerald-500/10 hover:text-emerald-600 dark:text-emerald-400 dark:hover:text-emerald-400"
            onClick={() => onDecide(item.id, "allowed", taskTool)}
          >
            <CircleCheckIcon /> 允许
          </Button>
          <Button variant="destructive" disabled={busy} onClick={() => onDecide(item.id, "denied")}>
            <CircleXIcon /> 拒绝
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}

// StatCard 是右列的数字卡：danger=true 时红色（blocked 意图 / 硬拦非零）。
function StatCard({ label, value, hint, danger }: { label: string; value: number; hint?: string; danger?: boolean }) {
  return (
    <div className={cn("rounded-lg border bg-background/40 p-3", danger && "border-red-500/40 bg-red-500/5")}>
      <p className="text-[11px] text-muted-foreground">{label}</p>
      <p className={cn("font-semibold text-2xl tabular-nums", danger ? "text-red-500" : "text-foreground")}>{value}</p>
      {hint && <p className="mt-0.5 text-[10px] text-muted-foreground">{hint}</p>}
    </div>
  );
}

// Column 是三列共用的外壳：标题行 + 可滚动内容区。移动端限高，md 起吃满整列。
// v0.10.1 用户拍板「太长了，左右缩小」：三列并排从 md 就生效（原 lg 才并排，
// 窄屏退化单列把页面拉超长）；min-w-0 必须有——grid 子项默认 min-width:auto
// 会拒绝收缩、把整页撑出横向滚动。
function Column({
  icon,
  title,
  extra,
  right,
  children,
}: {
  icon: React.ReactNode;
  title: string;
  extra?: React.ReactNode;
  right?: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <section className="flex max-h-[70svh] min-h-0 min-w-0 flex-col overflow-hidden rounded-xl border bg-card/40 md:max-h-none">
      <header className="flex shrink-0 items-center gap-1.5 border-b px-2 py-1.5 sm:gap-2 sm:px-3 sm:py-2">
        {icon}
        <h2 className="truncate font-semibold text-sm">{title}</h2>
        {extra}
        {right && <span className="ml-auto flex shrink-0 items-center gap-1.5">{right}</span>}
      </header>
      <div className="min-h-0 min-w-0 flex-1 overflow-y-auto p-1.5 sm:p-2">{children}</div>
    </section>
  );
}

// RecentList 是左列的渲染：加载中 / 空态 / 行列表（与决策账本页 DecisionList 同一套结构）。
function RecentList({ rows, loaded }: { rows: SodFeedRow[]; loaded: boolean }) {
  if (!loaded) {
    return <p className="py-6 text-center text-muted-foreground text-sm">加载中…</p>;
  }
  if (rows.length === 0) {
    return (
      <p className="py-6 text-center text-muted-foreground text-sm">还没有任何裁决——跑起来一个任务后这里会滚动起来。</p>
    );
  }
  return (
    <div className="grid gap-1.5">
      {rows.map((row) => (
        <FeedRow key={row.id} row={row} />
      ))}
    </div>
  );
}

// PendingList 是中列的渲染：加载中 / 空态 / 卡片列表。decidingId 对应的卡按钮置灰。
function PendingList({
  items,
  loaded,
  decidingId,
  onDecide,
}: {
  items: SodFeedPending[];
  loaded: boolean;
  decidingId: number | null;
  onDecide: (id: number, decision: "allowed" | "denied", taskTool?: boolean) => void;
}) {
  if (!loaded) {
    return <p className="py-6 text-center text-muted-foreground text-sm">加载中…</p>;
  }
  if (items.length === 0) {
    return (
      <Empty className="border-none">
        <EmptyHeader>
          <EmptyTitle>没有待人工裁决的调用</EmptyTitle>
          <EmptyDescription>
            全部自动处理中——只有命中「转人工」边界的调用才会进这里，点允许/拒绝后立即落账消失。
          </EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  }
  return (
    <div className="grid gap-2">
      {items.map((item) => (
        <PendingCard key={item.id} item={item} busy={decidingId === item.id} onDecide={onDecide} />
      ))}
    </div>
  );
}

// StatsPanel 是右列的渲染：加载中 / 数字卡组。blocked 意图与硬拦非零时红色。
function StatsPanel({ stats, loaded }: { stats: SodFeedStats | null | undefined; loaded: boolean }) {
  if (!loaded || !stats) {
    return <p className="py-6 text-center text-muted-foreground text-sm">加载中…</p>;
  }
  return (
    <div className="grid gap-2">
      <div className="grid grid-cols-2 gap-2">
        <StatCard label="自动放行 · 近1h" value={stats.auto_pass_1h} hint="快路径 + auditor pass" />
        <StatCard label="auditor 拒 · 近1h" value={stats.auditor_deny_1h} hint="auditor 口径拒绝" />
        <StatCard label="转人工 · 近1h" value={stats.human_1h} hint="ask 升级等待点头" />
        <StatCard
          label="硬拦 · 近1h"
          value={stats.hard_deny_1h}
          hint="删除/基线/种类硬拒"
          danger={stats.hard_deny_1h > 0}
        />
      </div>
      <StatCard
        label="人工改判 · 近1h"
        value={stats.override_1h}
        hint={stats.override_1h > 0 ? "有人工放行介入，裁决流里带「人工改判」徽标" : "近 1h 没有人工改判"}
      />
      <StatCard
        label="blocked 意图"
        value={stats.blocked_intents}
        hint={stats.blocked_intents > 0 ? "有意图被裁决挡住，去任务详情处理" : "没有意图被挡"}
        danger={stats.blocked_intents > 0}
      />
      <p className="mt-1 flex items-start gap-1.5 rounded-lg border border-border/60 bg-background/40 p-2.5 text-[11px] text-muted-foreground">
        <ShieldAlertIcon className="mt-0.5 size-3.5 shrink-0" />
        中列每张卡都是一次真实的等待：允许 / 拒绝走的是审批队列同一条 decide 通道，裁决全程落账可审计。
      </p>
    </div>
  );
}

export default function SodScreenPage() {
  const [feed, setFeed] = React.useState<SodFeed | null>(null);
  const [loaded, setLoaded] = React.useState(false);
  const [syncedAt, setSyncedAt] = React.useState("");
  const [visible, setVisible] = React.useState(true);
  const [decidingId, setDecidingId] = React.useState<number | null>(null);

  // 本地已决策的 pending id：点击后立即移除卡片；2s 轮询回来时按 id 去重，
  // 避免后端落账前的窗口把已裁决的卡又带回列表（轮询自然对账，无需手动刷新）。
  const decidedRef = React.useRef<Set<number>>(new Set());
  // 首屏失败报一次，之后轮询失败静默（下一轮自动重试）——与决策账本页同一套做法。
  const reportedRef = React.useRef(false);

  const load = React.useCallback(async () => {
    try {
      const r = await api.sodFeed();
      setFeed({
        recent: (r.recent ?? []).slice(0, MAX_RECENT),
        pending: (r.pending ?? []).filter((p) => !decidedRef.current.has(p.id)),
        stats: {
          auto_pass_1h: r.stats?.auto_pass_1h ?? 0,
          auditor_deny_1h: r.stats?.auditor_deny_1h ?? 0,
          human_1h: r.stats?.human_1h ?? 0,
          hard_deny_1h: r.stats?.hard_deny_1h ?? 0,
          override_1h: r.stats?.override_1h ?? 0,
          blocked_intents: r.stats?.blocked_intents ?? 0,
        },
        mode: r.mode ?? "off",
      });
      setSyncedAt(new Date().toLocaleTimeString("zh-CN", { hour12: false }));
      reportedRef.current = false;
    } catch (e) {
      if (!reportedRef.current) {
        reportedRef.current = true;
        toast.error(`读取裁决 feed 失败：${(e as Error).message}`);
      }
    } finally {
      setLoaded(true);
    }
  }, []);

  React.useEffect(() => {
    void load();
    const t = setInterval(() => {
      // 页面不可见（切走/最小化/挂墙休眠）时暂停轮询，切回时立即补一次对账。
      if (document.visibilityState !== "visible") return;
      void load();
    }, POLL_MS);
    const onVisibility = () => {
      const nowVisible = document.visibilityState === "visible";
      setVisible(nowVisible);
      if (nowVisible) void load();
    };
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      clearInterval(t);
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [load]);

  const decide = React.useCallback(async (id: number, decision: "allowed" | "denied", taskTool = false) => {
    setDecidingId(id);
    try {
      const r = await api.interceptDecide(id, decision, taskTool);
      decidedRef.current.add(id);
      setFeed((prev) => (prev ? { ...prev, pending: prev.pending.filter((p) => p.id !== id) } : prev));
      if (decision === "denied") {
        toast.success(`已拒绝 #${id}：worker 将收到拒绝`);
      } else if (r.task_tool_issued) {
        toast.success(`已允许 #${id}：本次与本任务后续同类调用都将放行（随任务终态过期）`);
      } else {
        toast.success(`已允许 #${id}：worker 重试同一调用将放行`);
      }
    } catch (e) {
      toast.error(`决策失败：${(e as Error).message}`);
    } finally {
      setDecidingId(null);
    }
  }, []);

  const recent = feed?.recent ?? [];
  const pending = feed?.pending ?? [];
  const stats = feed?.stats;
  const mode = feed?.mode ?? "off";

  return (
    <div
      data-content-padding="false"
      className="dark flex min-w-0 flex-col gap-3 bg-background p-3 sm:p-4 md:gap-4 md:h-[calc(100svh-3rem)] md:overflow-hidden md:p-4"
    >
      {/* 顶栏：标题 + LIVE 指示 + 返回入口 */}
      <header className="flex shrink-0 flex-wrap items-center gap-x-4 gap-y-2">
        <div className="min-w-0">
          <h1 className="flex items-center gap-2 font-semibold text-lg tracking-tight">
            <MonitorPlayIcon className="size-5 text-emerald-500" /> 职责分离 · 实时裁决大屏
          </h1>
          <p className="text-muted-foreground text-xs">
            左：裁决流 · 中：待人工裁决 · 右：统计 —— 每 2 秒对账
            {syncedAt ? ` · 更新于 ${syncedAt}` : ""}
          </p>
        </div>
        <div className="ml-auto flex items-center gap-2">
          <span
            className={cn(
              "inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-[11px]",
              visible
                ? "border-emerald-500/20 bg-emerald-500/10 text-emerald-600 dark:text-emerald-400"
                : "border-amber-500/20 bg-amber-500/10 text-amber-600 dark:text-amber-400",
            )}
          >
            <span className={cn("size-1.5 rounded-full", visible ? "animate-pulse bg-emerald-500" : "bg-amber-500")} />
            {visible ? "LIVE" : "已暂停 · 页面不可见"}
          </span>
          <Button asChild variant="ghost" size="sm">
            <Link href="/sod">
              <ArrowLeftIcon /> 返回 SoD
            </Link>
          </Button>
        </div>
      </header>

      <div className="grid min-h-0 min-w-0 flex-1 grid-cols-1 gap-3 md:grid-cols-3 md:gap-3 lg:gap-4">
        {/* 左列 · 实时裁决流 */}
        <Column
          icon={<ActivityIcon className="size-4 text-emerald-500" />}
          title="实时裁决流"
          extra={
            <Badge variant="outline" className={cn("text-[10px]", MODE_META[mode].badge)}>
              {MODE_META[mode].label}
            </Badge>
          }
          right={<span className="text-[11px] text-muted-foreground tabular-nums">{recent.length} 条</span>}
        >
          <RecentList rows={recent} loaded={loaded} />
        </Column>

        {/* 中列 · 待人工裁决（核心交互区） */}
        <Column
          icon={<GavelIcon className="size-4 text-amber-500" />}
          title="待人工裁决"
          extra={
            pending.length > 0 ? (
              <Badge
                variant="outline"
                className="border-amber-500/20 bg-amber-500/15 text-[10px] text-amber-600 dark:text-amber-400"
              >
                {pending.length} 张卡
              </Badge>
            ) : undefined
          }
        >
          <PendingList
            items={pending}
            loaded={loaded}
            decidingId={decidingId}
            onDecide={(id, d, taskTool) => void decide(id, d, taskTool)}
          />
        </Column>

        {/* 右列 · 统计与上下文 */}
        <Column icon={<PieChartIcon className="size-4 text-violet-500" />} title="统计与上下文">
          <StatsPanel stats={stats} loaded={loaded} />
        </Column>
      </div>
    </div>
  );
}
