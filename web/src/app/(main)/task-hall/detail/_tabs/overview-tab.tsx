"use client";

import Link from "next/link";

import { StatusBadge } from "@/components/status-badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Separator } from "@/components/ui/separator";
import type { Settings, Task, TaskHallGroupDetail, TaskHallQuota, TaskNode } from "@/lib/types";

import { QuotaMeter } from "../../_components/quota-meter";
import { fmtDateTime, fmtTokens, workerMembers } from "../../_components/task-hall-format";

// hallStatusTone 把状态档位映射成徽标色（未知档位给中性色）。
function hallStatusTone(level: string): string {
  if (/ok|normal|healthy|idle/i.test(level)) return "text-emerald-600 dark:text-emerald-400";
  if (/warn|degrad|slow|backlog/i.test(level)) return "text-amber-600 dark:text-amber-400";
  if (/block|error|crit|fail|stall/i.test(level)) return "text-red-600 dark:text-red-400";
  return "text-muted-foreground";
}

// OverviewTab 是大厅的仲裁面总览：成员席位、并发占用、额度进度、消息箱。
export function OverviewTab({
  detail,
  quota,
  intents,
  task,
  tasks,
  settings,
}: {
  detail: TaskHallGroupDetail;
  quota: TaskHallQuota | null;
  intents: TaskNode[];
  task?: Task;
  tasks: Task[];
  settings: Settings | null;
}) {
  const workers = workerMembers(detail.members);
  // 组内并发占用的分母口径与后端一致：这张探索图上 state='running' 的意图数。
  const groupUsed = intents.filter((n) => n.state === "running").length;
  const globalEnabled = settings?.task_concurrency_enabled === true;
  const globalLimit = settings?.task_concurrency_limit ?? 0;
  const globalUsed = tasks.filter((t) => t.status === "running").length;

  return (
    <div className="flex flex-col gap-4">
      {detail.hall_status ? (
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <span className="font-medium">大厅状态</span>
          <span className={`font-semibold ${hallStatusTone(detail.hall_status.level)}`}>{detail.hall_status.level}</span>
          <span className="text-muted-foreground">{detail.hall_status.summary}</span>
          {detail.hall_status.blocked_members?.length > 0 && (
            <span className="text-red-600 dark:text-red-400">
              卡住：{detail.hall_status.blocked_members.join("、")}
            </span>
          )}
        </div>
      ) : null}
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">成员席位</p>
            <p className="font-semibold text-lg tabular-nums">{workers.length}</p>
            <p className="text-muted-foreground text-xs">auto 单独作为分发者</p>
          </CardContent>
        </Card>
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">组内并发</p>
            <p className="font-semibold text-lg tabular-nums">
              {groupUsed}
              <span className="text-base text-muted-foreground"> / {detail.concurrency_limit}</span>
            </p>
            <p className="text-muted-foreground text-xs">在跑意图 / 上限</p>
          </CardContent>
        </Card>
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">额度剩余</p>
            <p className="font-semibold text-lg tabular-nums">{quota ? fmtTokens(quota.remaining_tokens) : "—"}</p>
            <p className="text-muted-foreground text-xs">{quota ? `周期 ${quota.period_key}` : "未配置额度"}</p>
          </CardContent>
        </Card>
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">消息箱</p>
            <p className="font-semibold text-lg tabular-nums">{detail.message_count}</p>
            <p className="text-muted-foreground text-xs">含已读与被拦截的</p>
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>额度进度</CardTitle>
          <CardDescription>
            {quota ? `自然日周期（${quota.period_kind}）· 状态` : "该群未配置额度 —— 没有额度行时模型调用一律被拒"}
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          {quota ? (
            <>
              <div className="flex items-center gap-2">
                <StatusBadge domain="quota" value={quota.state} dot />
                <span className="text-muted-foreground text-xs">周期 {quota.period_key}</span>
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
              <p className="text-muted-foreground text-xs">
                额度 {fmtTokens(quota.budget_tokens)} · 收尾（draining）与冻结（parked）都拒绝发起新的模型调用，
                在途预扣照常结算。
              </p>
            </>
          ) : (
            <p className="text-muted-foreground text-sm">在「额度」页可以给这个群开额度并设定收尾 / 冻结状态。</p>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>并发占用</CardTitle>
          <CardDescription>组内上限按这张探索图上在跑的意图判定；全局槽位限制同时运行的任务数</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="rounded-lg border p-3">
              <p className="text-muted-foreground text-xs">组内</p>
              <p className="font-semibold text-lg tabular-nums">
                {groupUsed}
                <span className="text-base text-muted-foreground"> / {detail.concurrency_limit}</span>
              </p>
              <p className="text-muted-foreground text-xs">在跑的意图 / 群并发上限</p>
            </div>
            <div className="rounded-lg border p-3">
              <p className="text-muted-foreground text-xs">全局槽位</p>
              <p className="font-semibold text-lg tabular-nums">
                {globalUsed}
                <span className="text-base text-muted-foreground">
                  {globalEnabled ? ` / ${globalLimit}` : " / 不限"}
                </span>
              </p>
              <p className="text-muted-foreground text-xs">
                {globalEnabled ? "运行中任务 / 任务并发上限" : "任务并发上限未开启"}
              </p>
            </div>
          </div>
          <Separator />
          <div className="grid gap-1 text-xs">
            <div className="flex items-center gap-2">
              <span className="w-20 shrink-0 text-muted-foreground">任务</span>
              {task ? (
                <Link href={`/function/tasks/detail?id=${task.id}`} className="truncate hover:text-primary">
                  {task.name || task.description || `任务 ${task.id}`}
                </Link>
              ) : (
                <span>任务 {detail.task_id}</span>
              )}
            </div>
            <div className="flex items-center gap-2">
              <span className="w-20 shrink-0 text-muted-foreground">任务状态</span>
              {task ? <StatusBadge domain="task" value={task.status} /> : <span>—</span>}
            </div>
            <div className="flex items-center gap-2">
              <span className="w-20 shrink-0 text-muted-foreground">建群时间</span>
              <span>{fmtDateTime(detail.created_at)}</span>
            </div>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
