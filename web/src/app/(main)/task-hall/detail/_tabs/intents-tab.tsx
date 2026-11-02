"use client";

import * as React from "react";

import { StatusBadge } from "@/components/status-badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import type { TaskNode } from "@/lib/types";

import { fmtDateTime } from "../../_components/task-hall-format";

const ALL = "all";

// intentSummary pulls the intent's headline out of its raw payload JSON.
function intentSummary(n: TaskNode): string {
  const raw = n.payload ?? "";
  if (!raw.trim()) return `Intent ${n.id}`;
  try {
    const parsed: unknown = JSON.parse(raw);
    if (parsed && typeof parsed === "object") {
      const summary = (parsed as Record<string, unknown>).summary;
      if (typeof summary === "string" && summary.trim()) return summary.trim();
    }
  } catch {
    return raw;
  }
  return raw;
}

// IntentsTab 是 Intent 看板：把「谁在做什么」摊开。
//
// 认领本身是后端原子完成的（ClaimIntent 的 CAS：state open→running + 写 owner），
// 所以前端只负责把状态摊开给人看。
export function IntentsTab({ intents }: { intents: TaskNode[] }) {
  const [state, setState] = React.useState(ALL);

  const counts = React.useMemo(() => {
    const map = new Map<string, number>();
    for (const n of intents) map.set(n.state, (map.get(n.state) ?? 0) + 1);
    return map;
  }, [intents]);

  const states = React.useMemo(() => Array.from(counts.keys()).sort(), [counts]);
  const visible = React.useMemo(
    () => (state === ALL ? intents : intents.filter((n) => n.state === state)),
    [intents, state],
  );

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <CardHeader>
          <CardTitle>Intent 看板</CardTitle>
          <CardDescription>
            共 {intents.length} 条意图 · 认领由后端原子完成（open → running），这里只做展示
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          <div className="flex flex-wrap items-center gap-2">
            <Select value={state} onValueChange={setState}>
              <SelectTrigger size="sm" className="w-40">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={ALL}>全部状态（{intents.length}）</SelectItem>
                {states.map((s) => (
                  <SelectItem key={s} value={s}>
                    {s}（{counts.get(s) ?? 0}）
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          {visible.length === 0 ? (
            <Empty className="border border-dashed">
              <EmptyHeader>
                <EmptyTitle>{intents.length === 0 ? "该任务还没有意图" : "没有符合筛选条件的意图"}</EmptyTitle>
                <EmptyDescription>
                  {intents.length === 0
                    ? "意图由 planner 在任务推进过程中生成；大厅刚建好时这里是空的。"
                    : "换一个状态再看看。"}
                </EmptyDescription>
              </EmptyHeader>
            </Empty>
          ) : (
            <Table className="**:data-[slot='table-cell']:px-4 **:data-[slot='table-head']:px-4">
              <TableHeader className="[&_tr]:border-t">
                <TableRow>
                  <TableHead>意图</TableHead>
                  <TableHead>状态</TableHead>
                  <TableHead>归属</TableHead>
                  <TableHead>优先级</TableHead>
                  <TableHead>来源</TableHead>
                  <TableHead>时间</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {visible.map((n) => (
                  <TableRow key={n.id}>
                    <TableCell>
                      <div className="flex flex-col gap-0.5">
                        <span className="max-w-md truncate font-medium">{intentSummary(n)}</span>
                        <span className="font-mono text-muted-foreground text-xs">#{n.id}</span>
                      </div>
                    </TableCell>
                    <TableCell>
                      <StatusBadge domain="intent" value={n.state} dot />
                    </TableCell>
                    <TableCell>
                      <span className="text-muted-foreground text-xs">—</span>
                    </TableCell>
                    <TableCell>
                      <span className="text-xs tabular-nums">{n.priority}</span>
                    </TableCell>
                    <TableCell>
                      <span className="text-muted-foreground text-xs">{n.origin || "—"}</span>
                    </TableCell>
                    <TableCell>
                      <span className="text-muted-foreground text-xs">{fmtDateTime(n.ts)}</span>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}

          <p className="text-muted-foreground text-xs">
            「归属」一列暂时为空：意图归属的唯一权威是探索图的 owner 字段（认领时原子写入），
            但当前意图列表接口没有返回该列，所以前端拿不到「谁认领了什么」。
          </p>
        </CardContent>
      </Card>
    </div>
  );
}
