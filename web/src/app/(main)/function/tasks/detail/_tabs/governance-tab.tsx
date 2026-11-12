"use client";

// 任务详情「治理」tab（方案 v10 §9 E1-6/E1-8）：本任务的裁决流 / 人工改判凭据 /
// 系统事件 + 生效 hook 链，一次看清「这台任务的每一次工具调用都被谁、按什么规则管住」。
// 数据来自只读聚合 GET /api/tasks/{id}/governance 与 GET /api/sod/hookchain，没有任何写入口。

import * as React from "react";

import Link from "next/link";

import { GavelIcon, KeyRoundIcon, Link2Icon, LockIcon, ScrollTextIcon } from "lucide-react";
import { toast } from "sonner";

import { StatusBadge } from "@/components/status-badge";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { api } from "@/lib/api";
import type { SodGovernanceDecision, SodGovernanceOverride, SodHookChain, SodSystemEvent } from "@/lib/types";

// NOTE_LIMIT 是裁决流 note 的默认截断长度：60 字符足够辨认这次调用，全文靠行内「展开/收起」。
const NOTE_LIMIT = 60;

// OVERRIDE_SCOPE_META 把改判凭据的 scope 翻成人话（后端 db.SodManualOverride 的三档）。
const OVERRIDE_SCOPE_META: Record<string, string> = {
  once: "单次 · 用后即焚",
  task: "本任务内 · 随终态过期",
  task_tool: "本任务同类通配 · 随终态过期",
};

// CATEGORY_ACTION_META 把种类动作翻成人话 + 徽标色（后端 sodpolicy 的四档动作）。
const CATEGORY_ACTION_META: Record<string, { label: string; className: string }> = {
  pass: { label: "放行", className: "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400" },
  auditor: { label: "审计", className: "bg-sky-500/15 text-sky-600 dark:text-sky-400" },
  ask: { label: "转人工", className: "bg-amber-500/15 text-amber-600 dark:text-amber-400" },
  hard_deny: { label: "硬拦", className: "bg-red-500/15 text-red-600 dark:text-red-400" },
};

// HOOK_ROUTE_BY_ENDPOINT 把 hook 链里可热改面的配置端点映射到对应的配置页面。
// 没有页面的（治理工具 sod_adjust_cap）不在表里，渲染为纯文本不可点。
// E1-8 勘误：种类表编辑器实际在 /system/intercept（拦截页同层），后端已改发该端点。
const HOOK_ROUTE_BY_ENDPOINT: Record<string, string> = {
  "/system/intercept": "/system/intercept",
  "/system/agents": "/system/agents",
};

// DecisionNote 渲染一条裁决的 note：默认截 60 字符，超长时给「展开/收起」行内开关。
function DecisionNote({ note, expanded, onToggle }: { note: string; expanded: boolean; onToggle: () => void }) {
  if (!note) return <span className="text-muted-foreground">—</span>;
  const truncated = note.length > NOTE_LIMIT;
  return (
    <span className="whitespace-pre-wrap break-words">
      {expanded || !truncated ? note : `${note.slice(0, NOTE_LIMIT)}…`}
      {truncated && (
        <button type="button" className="ml-1 text-primary underline-offset-4 hover:underline" onClick={onToggle}>
          {expanded ? "收起" : "展开"}
        </button>
      )}
    </span>
  );
}

// DecisionRow 是裁决流一行：时间 + 工具 + 裁决徽标（含人工改判标记）+ note。
function DecisionRow({
  row,
  expanded,
  onToggle,
}: {
  row: SodGovernanceDecision;
  expanded: boolean;
  onToggle: () => void;
}) {
  return (
    <TableRow>
      <TableCell className="whitespace-nowrap">
        <div className="flex flex-col items-start gap-0.5">
          <span className="text-xs tabular-nums">{row.ts}</span>
          <span className="font-mono text-muted-foreground text-xs">#{row.id}</span>
        </div>
      </TableCell>
      <TableCell>
        <div className="flex flex-col items-start gap-1">
          <span className="font-mono text-xs">{row.tool || "—"}</span>
          <div className="flex flex-wrap items-center gap-1">
            <span className="text-muted-foreground text-xs">{row.agent || "—"}</span>
            {row.category && (
              <span className="rounded border border-border/60 bg-muted/40 px-1 py-px text-[10px] text-muted-foreground">
                {row.category}
              </span>
            )}
          </div>
        </div>
      </TableCell>
      <TableCell>
        <div className="flex flex-col items-start gap-1">
          <StatusBadge domain="sodVerdict" value={row.verdict} dot />
          <StatusBadge domain="sodReason" value={row.reason_code} />
          {!!row.overridden && (
            <Badge
              variant="outline"
              className="border-violet-500/30 bg-violet-500/15 px-1.5 py-0 text-[10px] text-violet-600 dark:text-violet-400"
            >
              人工改判
            </Badge>
          )}
        </div>
      </TableCell>
      <TableCell className="max-w-md">
        <span className="text-xs">
          <DecisionNote note={row.note} expanded={expanded} onToggle={onToggle} />
        </span>
      </TableCell>
    </TableRow>
  );
}

// OverrideRow 是人工改判凭据一行：工具 + 范围 + 理由 + 操作者（+ 凭据摘要）。
function OverrideRow({ row }: { row: SodGovernanceOverride }) {
  return (
    <TableRow>
      <TableCell>
        <span className="font-mono text-xs">{row.tool || "—"}</span>
      </TableCell>
      <TableCell>
        <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
          {OVERRIDE_SCOPE_META[row.scope] ?? row.scope}
        </Badge>
      </TableCell>
      <TableCell className="max-w-sm">
        <span className="break-words text-muted-foreground text-xs">{row.reason || "—"}</span>
      </TableCell>
      <TableCell>
        <span className="text-xs">{row.created_by || "—"}</span>
      </TableCell>
    </TableRow>
  );
}

// SystemEventRow 是系统事件一行：时间 + 来源 + 类型 + 摘要。
function SystemEventRow({ row }: { row: SodSystemEvent }) {
  return (
    <TableRow>
      <TableCell className="whitespace-nowrap text-xs tabular-nums">{row.ts}</TableCell>
      <TableCell>
        <span className="font-mono text-muted-foreground text-xs">{row.worker || "—"}</span>
      </TableCell>
      <TableCell>
        <span className="font-mono text-xs">{row.kind || "—"}</span>
      </TableCell>
      <TableCell className="max-w-md">
        <span className="break-words text-xs">{row.summary || "—"}</span>
      </TableCell>
    </TableRow>
  );
}

export function GovernanceTab({ taskId }: { taskId: string }) {
  const [decisions, setDecisions] = React.useState<SodGovernanceDecision[]>([]);
  // M2/M4（方案 v10 §6 二期）：唤醒计次与 intent 观测计数（governance 载荷扩展字段）。
  const [wakeStats, setWakeStats] = React.useState<{ total?: number; edge?: number; heartbeat?: number } | null>(null);
  const [intentCounts, setIntentCounts] = React.useState<{ attempts_total?: number; outcomes?: unknown[] } | null>(
    null,
  );
  const [overrides, setOverrides] = React.useState<SodGovernanceOverride[]>([]);
  const [systemEvents, setSystemEvents] = React.useState<SodSystemEvent[]>([]);
  const [loaded, setLoaded] = React.useState(false);
  const [hook, setHook] = React.useState<SodHookChain | null>(null);
  const [hookLoaded, setHookLoaded] = React.useState(false);
  // note 展开的裁决 id 集合：默认截 60 字符，点「展开」看全文。
  const [expandedNotes, setExpandedNotes] = React.useState<Set<number>>(() => new Set());

  const toggleNote = React.useCallback((id: number) => {
    setExpandedNotes((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  React.useEffect(() => {
    let alive = true;
    setLoaded(false);
    api
      .sodGovernance(taskId)
      .then((r) => {
        if (!alive) return;
        setDecisions(r.decisions);
        setOverrides(r.overrides);
        setSystemEvents(r.system_events);
        setWakeStats(
          ((r as Record<string, unknown>).wake_stats as
            | { total?: number; edge?: number; heartbeat?: number }
            | undefined) ?? null,
        );
        setIntentCounts(
          ((r as Record<string, unknown>).intent_counts as
            | { attempts_total?: number; outcomes?: unknown[] }
            | undefined) ?? null,
        );
      })
      .catch((e) => {
        if (alive) toast.error(`读取任务治理视图失败：${(e as Error).message}`);
      })
      .finally(() => {
        if (alive) setLoaded(true);
      });
    return () => {
      alive = false;
    };
  }, [taskId]);

  React.useEffect(() => {
    let alive = true;
    api
      .hookChain()
      .then((r) => {
        if (alive) setHook(r);
      })
      .catch((e) => {
        if (alive) toast.error(`读取生效 hook 链失败：${(e as Error).message}`);
      })
      .finally(() => {
        if (alive) setHookLoaded(true);
      });
    return () => {
      alive = false;
    };
  }, []);

  if (!loaded) {
    return <p className="py-6 text-center text-muted-foreground text-sm">加载中…</p>;
  }

  return (
    <div className="flex flex-col gap-4">
      {/* ⓪ 观测计数（M2/M4，方案 v10 §6 二期）：唤醒计次 + 意图观测 */}
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-sm">观测计数</CardTitle>
          <CardDescription>
            唤醒按触发来源计次（观测面，先只记不拦）；意图认领次数与终态序列来自 M4 观测列
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-wrap items-center gap-x-6 gap-y-2 text-sm">
          {wakeStats && (
            <span className="text-muted-foreground">
              唤醒总计 <span className="text-foreground font-semibold">{wakeStats.total ?? 0}</span>（图变{" "}
              {wakeStats.edge ?? 0} · 心跳 {wakeStats.heartbeat ?? 0}）
            </span>
          )}
          {intentCounts && (
            <span className="text-muted-foreground">
              意图认领 <span className="text-foreground font-semibold">{intentCounts.attempts_total ?? 0}</span> 次
            </span>
          )}
          {!wakeStats && !intentCounts && <span className="text-muted-foreground">暂无观测数据</span>}
        </CardContent>
      </Card>
      {/* ① 裁决流：本任务的 sod_decisions（近 100 条，倒序） */}
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-sm">
            <GavelIcon className="size-4 text-amber-500" /> 裁决流
          </CardTitle>
          <CardDescription>
            本任务最近 {decisions.length} 条工具裁决 · 放行 / 拒绝 / 待证伪 · 带人工改判标记的行说明这次放行来自人工复核
          </CardDescription>
        </CardHeader>
        <CardContent>
          {decisions.length === 0 ? (
            <p className="py-6 text-center text-muted-foreground text-sm">
              本任务还没有裁决记录——闸门只对真的发生过的工具调用记账。
            </p>
          ) : (
            <div className="overflow-hidden rounded-md border">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className="w-24">时间</TableHead>
                    <TableHead>工具</TableHead>
                    <TableHead className="w-36">裁决</TableHead>
                    <TableHead>说明</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {decisions.map((row) => (
                    <DecisionRow
                      key={row.id}
                      row={row}
                      expanded={expandedNotes.has(row.id)}
                      onToggle={() => toggleNote(row.id)}
                    />
                  ))}
                </TableBody>
              </Table>
            </div>
          )}
        </CardContent>
      </Card>

      {/* ② 人工改判：本任务的 sod_manual_overrides 凭据 */}
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-sm">
            <KeyRoundIcon className="size-4 text-violet-500" /> 人工改判
          </CardTitle>
          <CardDescription>本任务当前有效的放行凭据 · 凭据是调用时点查的，过期 / 用后即焚的不会再出现</CardDescription>
        </CardHeader>
        <CardContent>
          {overrides.length === 0 ? (
            <p className="py-6 text-center text-muted-foreground text-sm">
              没有人工改判凭据——这个任务的放行全部来自闸门自动裁决。
            </p>
          ) : (
            <div className="overflow-hidden rounded-md border">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>工具</TableHead>
                    <TableHead className="w-52">放行范围</TableHead>
                    <TableHead>理由</TableHead>
                    <TableHead className="w-28">操作者</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {overrides.map((row) => (
                    <OverrideRow key={row.id} row={row} />
                  ))}
                </TableBody>
              </Table>
            </div>
          )}
        </CardContent>
      </Card>

      {/* ③ 系统事件：B6 升级 / LLM 故障转移等平台侧事件 */}
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-sm">
            <ScrollTextIcon className="size-4 text-blue-500" /> 系统事件
          </CardTitle>
          <CardDescription>平台侧对本任务的干预记录（意图升级 / LLM 切换与故障转移等），近 100 条</CardDescription>
        </CardHeader>
        <CardContent>
          {systemEvents.length === 0 ? (
            <p className="py-6 text-center text-muted-foreground text-sm">没有系统事件——运行平稳，平台没有介入过。</p>
          ) : (
            <div className="overflow-hidden rounded-md border">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className="w-24">时间</TableHead>
                    <TableHead className="w-28">来源</TableHead>
                    <TableHead className="w-32">类型</TableHead>
                    <TableHead>摘要</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {systemEvents.map((row) => (
                    <SystemEventRow key={`${row.kind}-${row.worker}-${row.ts}-${row.summary}`} row={row} />
                  ))}
                </TableBody>
              </Table>
            </div>
          )}
        </CardContent>
      </Card>

      {/* ④ 生效 hook 链（E1-8：看得见才能改）：可热改配置面 + 代码常量纪律面 */}
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-sm">
            <Link2Icon className="size-4 text-emerald-500" /> 生效 hook 链
          </CardTitle>
          <CardDescription>
            每次工具调用依次经过的面 · 可热改面指向既有配置入口；代码常量是安全纪律，不进热改面
          </CardDescription>
        </CardHeader>
        <CardContent>
          {!hookLoaded || !hook ? (
            <p className="py-6 text-center text-muted-foreground text-sm">加载中…</p>
          ) : (
            <div className="grid gap-4 lg:grid-cols-2">
              <div className="flex flex-col gap-2">
                <h3 className="font-medium text-muted-foreground text-xs">可热改（配置面）</h3>
                {hook.editable.map((item) => {
                  const route = HOOK_ROUTE_BY_ENDPOINT[item.endpoint];
                  return (
                    <div key={item.name} className="rounded-lg border p-2.5">
                      <div className="flex flex-wrap items-center gap-2">
                        <span className="font-medium text-sm">{item.name}</span>
                        {route && (
                          <Link href={route} className="text-primary text-xs underline-offset-4 hover:underline">
                            前往配置
                          </Link>
                        )}
                      </div>
                      <p className="mt-1 text-muted-foreground text-xs">{item.note}</p>
                    </div>
                  );
                })}
              </div>
              <div className="flex flex-col gap-2">
                <h3 className="font-medium text-muted-foreground text-xs">代码常量（只读）</h3>
                {hook.readonly.map((item) => (
                  <div
                    key={item.name}
                    className="rounded-lg border border-dashed bg-muted/30 p-2.5 text-muted-foreground"
                  >
                    <div className="flex items-center gap-1.5">
                      <LockIcon className="size-3.5 shrink-0" />
                      <span className="font-medium text-sm">{item.name}</span>
                    </div>
                    <p className="mt-1 text-xs">{item.reason}</p>
                  </div>
                ))}
              </div>
            </div>
          )}
        </CardContent>
      </Card>

      {/* ⑤ 生效种类表（E1-8 实况化）：sod_categories 实读，动作/cap/启停一目了然 */}
      {hook.categories && hook.categories.length > 0 && (
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-sm">
              <GavelIcon className="size-4 text-emerald-500" /> 生效种类表（实读）
            </CardTitle>
            <CardDescription>
              共 {hook.categories.length} 类 · 按优先级降序 · 动作与 cap 改后即时生效（前往上方「拦截种类」配置入口）
            </CardDescription>
          </CardHeader>
          <CardContent>
            <div className="max-h-72 overflow-y-auto rounded-md border">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className="w-36">种类</TableHead>
                    <TableHead className="w-20">动作</TableHead>
                    <TableHead className="w-16">cap/任务</TableHead>
                    <TableHead className="w-14">启停</TableHead>
                    <TableHead>理由</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {hook.categories.map((c) => {
                    const meta = CATEGORY_ACTION_META[c.action];
                    return (
                      <TableRow key={c.name}>
                        <TableCell className="font-medium">
                          {c.name}
                          {c.locked && <LockIcon className="ml-1 inline size-3 text-muted-foreground" />}
                        </TableCell>
                        <TableCell>
                          <span
                            className={`rounded px-1.5 py-0.5 text-xs ${meta?.className ?? "bg-muted text-muted-foreground"}`}
                          >
                            {meta?.label ?? c.action}
                          </span>
                        </TableCell>
                        <TableCell className="tabular-nums">{c.cap_per_task}</TableCell>
                        <TableCell>{c.enabled ? "启用" : "停用"}</TableCell>
                        <TableCell className="text-muted-foreground text-xs">{c.reason}</TableCell>
                      </TableRow>
                    );
                  })}
                </TableBody>
              </Table>
            </div>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
