"use client";

import * as React from "react";

import Link from "next/link";

import { UsersIcon } from "lucide-react";
import { toast } from "sonner";

import { StatusBadge } from "@/components/status-badge";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { api } from "@/lib/api";
import type { Task, TaskHallQuota, TaskHallRow } from "@/lib/types";

import { CreateGroupDialog } from "./_components/create-group-dialog";
import { QuotaMeter } from "./_components/quota-meter";
import { fmtDateTime, fmtTokens, workerMembers } from "./_components/task-hall-format";

// POLL_MS 是列表刷新周期：大厅的并发占用与额度都在动，页面要能看出变化。
const POLL_MS = 10_000;

function HallRow({ row, task, quota }: { row: TaskHallRow; task?: Task; quota: TaskHallQuota | null }) {
  const workers = workerMembers(row.members);
  // 在跑的意图数 = 该群 task 的运行中 worker 数（后端组内并发上限的分母）。
  const used = task?.in_flight ?? 0;

  return (
    <TableRow>
      <TableCell>
        <Link
          href={`/task-hall/detail?id=${row.group_id}`}
          className="flex flex-col gap-0.5 font-medium hover:text-primary"
        >
          <span className="truncate">{row.name}</span>
          <span className="font-mono text-muted-foreground text-xs">#{row.group_id}</span>
        </Link>
      </TableCell>
      <TableCell>
        <div className="flex flex-col gap-0.5">
          {task ? (
            <Link href={`/function/tasks/detail?id=${task.id}`} className="truncate hover:text-primary">
              {task.name || task.description || `任务 ${task.id}`}
            </Link>
          ) : (
            <span className="text-muted-foreground">任务 {row.task_id}</span>
          )}
          {task && <StatusBadge domain="task" value={task.status} />}
        </div>
      </TableCell>
      <TableCell>
        <div className="flex flex-col gap-1">
          <span className="text-xs">
            {workers.length} 位成员
            <span className="text-muted-foreground"> · auto 分发</span>
          </span>
          <div className="flex flex-wrap gap-1">
            {workers.slice(0, 4).map((m) => (
              <Badge key={m.agent_key} variant="outline" className="px-1.5 py-0 text-[10px]">
                {m.agent_key}
              </Badge>
            ))}
            {workers.length > 4 && (
              <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
                +{workers.length - 4}
              </Badge>
            )}
          </div>
        </div>
      </TableCell>
      <TableCell>
        <div className="flex flex-col gap-0.5">
          <span className="text-xs tabular-nums">
            {used} / {row.concurrency_limit}
          </span>
          <span className="text-muted-foreground text-xs">在跑意图 / 上限</span>
        </div>
      </TableCell>
      <TableCell>
        {quota ? (
          <div className="flex w-40 flex-col gap-1">
            <QuotaMeter
              consumed={quota.consumed_tokens}
              reserved={quota.reserved_tokens}
              budget={quota.budget_tokens}
            />
            <span className="text-muted-foreground text-xs tabular-nums">
              消费 {fmtTokens(quota.consumed_tokens)} · 预扣 {fmtTokens(quota.reserved_tokens)} · 剩余{" "}
              {fmtTokens(quota.remaining_tokens)}
            </span>
          </div>
        ) : (
          <span className="text-muted-foreground text-xs">未配置额度</span>
        )}
      </TableCell>
      <TableCell>
        <div className="flex flex-col items-start gap-1">
          <StatusBadge domain="group" value={row.status} dot />
          <span className="text-muted-foreground text-xs">{fmtDateTime(row.created_at)}</span>
        </div>
      </TableCell>
    </TableRow>
  );
}

function HallList({
  rows,
  taskByID,
  quotas,
  loaded,
}: {
  rows: TaskHallRow[];
  taskByID: Map<string, Task>;
  quotas: Record<number, TaskHallQuota | null>;
  loaded: boolean;
}) {
  if (!loaded) {
    return <p className="py-6 text-center text-muted-foreground text-sm">加载中…</p>;
  }
  if (rows.length === 0) {
    return (
      <Empty className="mx-4 border border-dashed lg:mx-6">
        <EmptyHeader>
          <EmptyTitle>还没有任务大厅</EmptyTitle>
          <EmptyDescription>
            大厅要挂在一个已有任务上：先在「任务」页建任务并让它跑起来，再回到这里为它选成员建大厅。
          </EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  }
  return (
    <Table className="**:data-[slot='table-cell']:px-4 **:data-[slot='table-head']:px-4">
      <TableHeader className="[&_tr]:border-t">
        <TableRow>
          <TableHead>大厅</TableHead>
          <TableHead>任务</TableHead>
          <TableHead>成员</TableHead>
          <TableHead>并发</TableHead>
          <TableHead>额度</TableHead>
          <TableHead>状态</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((row) => (
          <HallRow
            key={row.group_id}
            row={row}
            task={taskByID.get(String(row.task_id))}
            quota={quotas[row.group_id] ?? null}
          />
        ))}
      </TableBody>
    </Table>
  );
}

export default function TaskHallPage() {
  const [rows, setRows] = React.useState<TaskHallRow[]>([]);
  const [quotas, setQuotas] = React.useState<Record<number, TaskHallQuota | null>>({});
  const [tasks, setTasks] = React.useState<Task[]>([]);
  const [loaded, setLoaded] = React.useState(false);

  const load = React.useCallback(async () => {
    try {
      const [list, taskResp] = await Promise.all([api.taskHallTasks(), api.tasks()]);
      setRows(list);
      setTasks(taskResp.tasks);
      // 额度读数不在列表接口里（它按群单查），所以并发取一遍。
      const entries = await Promise.all(
        list.map(async (row) => [row.group_id, await api.taskHallQuota(row.group_id).catch(() => null)] as const),
      );
      setQuotas(Object.fromEntries(entries));
    } catch (e) {
      toast.error(`读取任务大厅失败：${(e as Error).message}`);
    } finally {
      setLoaded(true);
    }
  }, []);

  React.useEffect(() => {
    void load();
    const t = setInterval(() => void load(), POLL_MS);
    return () => clearInterval(t);
  }, [load]);

  const taskByID = React.useMemo(() => new Map(tasks.map((t) => [t.id, t])), [tasks]);

  const activeCount = rows.filter((r) => r.status === "active").length;
  const heldCount = rows.filter((r) => r.status === "draining" || r.status === "parked").length;
  const seatCount = rows.reduce((sum, r) => sum + workerMembers(r.members).length, 0);

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div className="flex items-start justify-between gap-3">
        <div>
          <h1 className="font-semibold text-xl tracking-tight">任务大厅</h1>
          <p className="text-muted-foreground text-sm">
            一个任务一个大厅 · 多 Agent 共享一张探索图 · 人在大厅里布置、仲裁与插话
          </p>
        </div>
        <CreateGroupDialog onCreated={() => void load()} />
      </div>

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">大厅</p>
            <p className="font-semibold text-lg tabular-nums">{rows.length}</p>
          </CardContent>
        </Card>
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">运行中</p>
            <p className="font-semibold text-lg tabular-nums">{activeCount}</p>
          </CardContent>
        </Card>
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">收尾 / 冻结</p>
            <p className="font-semibold text-lg tabular-nums">{heldCount}</p>
          </CardContent>
        </Card>
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">成员席位</p>
            <p className="font-semibold text-lg tabular-nums">{seatCount}</p>
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>大厅列表</CardTitle>
          <CardDescription>共 {rows.length} 个 · 点击进入可看成员、Intent 看板、额度与消息箱</CardDescription>
        </CardHeader>
        <CardContent>
          <HallList rows={rows} taskByID={taskByID} quotas={quotas} loaded={loaded} />
        </CardContent>
      </Card>

      <p className="text-muted-foreground text-xs">
        <UsersIcon className="mr-1 inline size-3.5 align-text-bottom" />
        一个任务至多一个大厅；重复建会被后端判为冲突。额度按自然日（Asia/Shanghai）重置。
      </p>
    </div>
  );
}
