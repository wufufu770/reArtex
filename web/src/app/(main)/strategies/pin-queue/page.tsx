"use client";

import * as React from "react";

import Link from "next/link";

import { EyeIcon, PinIcon, ShieldAlertIcon } from "lucide-react";
import { toast } from "sonner";

import { StatusBadge } from "@/components/status-badge";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { api } from "@/lib/api";
import type { Strategy, StrategyPinQueue } from "@/lib/types";

import { fmtDateTime, fmtScore } from "../_components/strategy-format";
import { StrategyNav } from "../_components/strategy-nav";

// PinQueueList 是复审清单的渲染：加载中 / 空态 / 表格三种。
function PinQueueList({ queue, loaded }: { queue: Strategy[]; loaded: boolean }) {
  if (!loaded) {
    return <p className="py-6 text-center text-muted-foreground text-sm">加载中…</p>;
  }
  if (queue.length === 0) {
    return (
      <Empty className="mx-4 border border-dashed lg:mx-6">
        <EmptyHeader>
          <EmptyTitle>队列是空的</EmptyTitle>
          <EmptyDescription>
            当前没有被 pin 的策略。pin 由 curator 的唯一证人判定或人工置位产生， 置位与解除都要写明理由与操作人。
          </EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  }
  return (
    <Table className="**:data-[slot='table-cell']:px-4 **:data-[slot='table-head']:px-4">
      <TableHeader className="[&_tr]:border-t">
        <TableRow>
          <TableHead>策略</TableHead>
          <TableHead>状态</TableHead>
          <TableHead className="hidden md:table-cell">效用</TableHead>
          <TableHead className="hidden lg:table-cell">观测</TableHead>
          <TableHead>更新</TableHead>
          <TableHead>复审</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {queue.map((s) => (
          <TableRow key={s.id}>
            <TableCell>
              <Link
                href={`/strategies/detail?id=${s.id}`}
                className="flex flex-col gap-0.5 font-medium hover:text-primary"
              >
                <span className="truncate">{s.name}</span>
                <span className="truncate font-normal text-muted-foreground text-xs">{s.summary || "（无摘要）"}</span>
              </Link>
            </TableCell>
            <TableCell>
              <div className="flex flex-col items-start gap-1">
                <StatusBadge domain="strategy" value={s.state} dot />
                <Badge variant="outline" className="gap-1 px-1.5 py-0 text-[10px]">
                  <PinIcon className="size-3" />
                  唯一证人
                </Badge>
              </div>
            </TableCell>
            <TableCell className="hidden md:table-cell">
              <span className="text-muted-foreground text-xs tabular-nums">{fmtScore(s.utility)}</span>
            </TableCell>
            <TableCell className="hidden lg:table-cell">
              <span className="text-muted-foreground text-xs tabular-nums">
                用 {s.usage_count} · 命中 {s.hit_count} · 未命中 {s.miss_count}
              </span>
            </TableCell>
            <TableCell>
              <span className="text-muted-foreground text-xs">{fmtDateTime(s.updated_at)}</span>
            </TableCell>
            <TableCell>
              <Button asChild size="sm" variant="outline">
                <Link href={`/strategies/detail?id=${s.id}`}>
                  <EyeIcon /> 去复审
                </Link>
              </Button>
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

export default function StrategyPinQueuePage() {
  const [data, setData] = React.useState<StrategyPinQueue | null>(null);
  const [loaded, setLoaded] = React.useState(false);

  const lastRef = React.useRef<string>("");
  const reportedRef = React.useRef(false);

  const load = React.useCallback(async () => {
    try {
      const q = await api.strategyPinQueue();
      const sig = JSON.stringify(q);
      if (sig === lastRef.current) return;
      lastRef.current = sig;
      setData(q);
      reportedRef.current = false;
    } catch (e) {
      if (!reportedRef.current) {
        reportedRef.current = true;
        toast.error(`读取 pin 复审队列失败：${(e as Error).message}`);
      }
    } finally {
      setLoaded(true);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  const queue = data?.queue ?? [];

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div>
        <h1 className="font-semibold text-xl tracking-tight">pin 复审队列</h1>
        <p className="text-muted-foreground text-sm">
          唯一证人保护的那批策略 · 本页只读：解 pin 只能由人在策略详情页显式做
        </p>
      </div>

      <StrategyNav current="pin-queue" />

      <Alert variant="default">
        <ShieldAlertIcon />
        <AlertTitle>这份队列只是提示，不会替你解 pin</AlertTitle>
        <AlertDescription>
          被 pin 的策略往往是通往某个 confirmed 漏洞的唯一已知路径 —— 自动解 pin
          会让那份漏洞报告再也复现不出来。所以队列永远只提示：
          要不要解、什么时候解，由人决定，并且必须写明理由与操作人。
        </AlertDescription>
      </Alert>

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3">
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">已 pin</p>
            <p className="font-semibold text-lg tabular-nums">{data?.pinned_count ?? "—"}</p>
          </CardContent>
        </Card>
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">复审阈值</p>
            <p className="font-semibold text-lg tabular-nums">{data?.threshold ?? "—"}</p>
          </CardContent>
        </Card>
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">是否堆积过线</p>
            <p className="font-semibold text-lg">{data?.review_required ? "是 · 该看一眼了" : "否"}</p>
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>复审清单</CardTitle>
          <CardDescription>共 {queue.length} 条 · 堆积过线只表示「该让人看一眼了」，不代表任何自动动作</CardDescription>
        </CardHeader>
        <CardContent>
          <PinQueueList queue={queue} loaded={loaded} />
        </CardContent>
      </Card>
    </div>
  );
}
