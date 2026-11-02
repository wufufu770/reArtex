"use client";

import * as React from "react";

import { CircleAlertIcon, SearchIcon } from "lucide-react";
import { toast } from "sonner";

import { EgressGovernanceNotice } from "@/components/egress-governance-notice";
import { StatusBadge } from "@/components/status-badge";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Input } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { api } from "@/lib/api";
import type { SodCircuit } from "@/lib/types";

import { fmtDateTime } from "../_components/sod-format";
import { SodNav } from "../_components/sod-nav";

// CircuitList 是熔断表的渲染：加载中 / 空态 / 表格三种。
function CircuitList({ rows, loaded }: { rows: SodCircuit[]; loaded: boolean }) {
  if (!loaded) {
    return <p className="py-6 text-center text-muted-foreground text-sm">加载中…</p>;
  }
  if (rows.length === 0) {
    return (
      <Empty className="mx-4 border border-dashed lg:mx-6">
        <EmptyHeader>
          <EmptyTitle>没有工具熔过</EmptyTitle>
          <EmptyDescription>
            熔断只影响已经出过事的工具，所以这张表通常很短。现在没有任何工具被熔断 ——
            每个工具都还在正常放行。想确认某个具体工具，用下面的单查。
          </EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  }
  return (
    <Table className="**:data-[slot='table-cell']:px-4 **:data-[slot='table-head']:px-4">
      <TableHeader className="[&_tr]:border-t">
        <TableRow>
          <TableHead>工具</TableHead>
          <TableHead>有效状态</TableHead>
          <TableHead>行内原始值</TableHead>
          <TableHead>连续失败</TableHead>
          <TableHead className="hidden md:table-cell">开路时间</TableHead>
          <TableHead className="hidden lg:table-cell">原因</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((c) => (
          <TableRow key={c.tool_key}>
            <TableCell>
              <div className="flex flex-col items-start gap-0.5">
                <span className="font-mono text-sm">{c.tool_key}</span>
                {c.updated_at && (
                  <span className="text-muted-foreground text-xs tabular-nums">更新于 {fmtDateTime(c.updated_at)}</span>
                )}
              </div>
            </TableCell>
            <TableCell>
              <StatusBadge domain="sodCircuit" value={c.state} dot />
            </TableCell>
            <TableCell>
              <span className="font-mono text-muted-foreground text-xs">{c.recorded_state || "—"}</span>
            </TableCell>
            <TableCell>
              <span className="text-xs tabular-nums">{c.fail_count}</span>
            </TableCell>
            <TableCell className="hidden md:table-cell">
              <span className="text-muted-foreground text-xs tabular-nums">
                {c.opened_at ? fmtDateTime(c.opened_at) : "—"}
              </span>
            </TableCell>
            <TableCell className="hidden lg:table-cell">
              <span className="max-w-md text-muted-foreground text-xs">{c.reason || "—"}</span>
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

export default function SodCircuitPage() {
  const [data, setData] = React.useState<{
    circuits: SodCircuit[];
    states: Record<string, number>;
    note: string;
  } | null>(null);
  const [loaded, setLoaded] = React.useState(false);

  const [query, setQuery] = React.useState("");
  const [probe, setProbe] = React.useState<SodCircuit | null>(null);
  const [probing, setProbing] = React.useState(false);

  const lastRef = React.useRef<string>("");
  const reportedRef = React.useRef(false);

  const load = React.useCallback(async () => {
    try {
      const q = await api.sodCircuits();
      const sig = JSON.stringify(q);
      if (sig === lastRef.current) return;
      lastRef.current = sig;
      setData(q);
      reportedRef.current = false;
    } catch (e) {
      if (!reportedRef.current) {
        reportedRef.current = true;
        toast.error(`读取工具熔断状态失败：${(e as Error).message}`);
      }
    } finally {
      setLoaded(true);
    }
  }, []);

  React.useEffect(() => {
    void load();
    const t = setInterval(() => void load(), 15_000);
    return () => clearInterval(t);
  }, [load]);

  async function probeTool() {
    const key = query.trim();
    if (!key) {
      toast.error("请填写工具 key");
      return;
    }
    setProbing(true);
    try {
      setProbe(await api.sodCircuit(key));
    } catch (e) {
      toast.error(`查询熔断状态失败：${(e as Error).message}`);
    } finally {
      setProbing(false);
    }
  }

  const circuits = data?.circuits ?? [];
  const states = data?.states ?? {};

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div>
        <h1 className="font-semibold text-xl tracking-tight">工具熔断</h1>
        <p className="text-muted-foreground text-sm">此刻哪些工具是关着的 · 熔断开路 = 闸门拒绝这个工具的一切调用</p>
      </div>

      <SodNav current="circuit" />

      <Alert variant="default">
        <CircleAlertIcon />
        <AlertTitle>熔断开路的工具现在是被拒绝的，不是「慢一点」</AlertTitle>
        <AlertDescription>
          连续失败到阈值的工具会被闸门开路：之后这个工具的一切调用都直接被拒，不进执行器。 开路超过一段时间后转入
          half_open —— 放行一次试探，成功就恢复、失败就重新开路。 表里的 state
          是闸门此刻实际执行的有效状态，recorded_state 是行里记着的原始值。
        </AlertDescription>
      </Alert>

      <EgressGovernanceNotice />

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3">
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">开路 · 拒绝一切调用</p>
            <p className="font-semibold text-lg tabular-nums">{states.open ?? 0}</p>
          </CardContent>
        </Card>
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">半开 · 放行一次试探</p>
            <p className="font-semibold text-lg tabular-nums">{states.half_open ?? 0}</p>
          </CardContent>
        </Card>
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">有记录的工具</p>
            <p className="font-semibold text-lg tabular-nums">{circuits.length}</p>
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>熔断清单</CardTitle>
          <CardDescription>
            共 {circuits.length} 条{data?.note ? ` · ${data.note}` : ""}
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          <CircuitList rows={circuits} loaded={loaded} />
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>单查一个工具</CardTitle>
          <CardDescription>
            清单只列「出过事」的工具。查一个没出事的工具，它就是 closed —— 不是「查不到」。
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          <div className="flex flex-wrap items-center gap-2">
            <div className="relative w-full sm:max-w-xs">
              <SearchIcon className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
              <Input
                placeholder="工具 key，例如 Bash"
                className="pl-8"
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") void probeTool();
                }}
              />
            </div>
            <Button variant="outline" size="sm" onClick={() => void probeTool()} disabled={probing}>
              {probing ? "查询中…" : "查询熔断状态"}
            </Button>
          </div>

          {probe && (
            <div className="flex flex-wrap items-center gap-2 rounded-lg border border-border bg-muted/30 px-3 py-2">
              <span className="font-mono text-sm">{probe.tool_key}</span>
              <StatusBadge domain="sodCircuit" value={probe.state} dot />
              <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
                连续失败 {probe.fail_count}
              </Badge>
              {!probe.present && (
                <span className="text-muted-foreground text-xs">表里没有这一行 ⇒ 这个工具还没出过事</span>
              )}
              {probe.reason && <span className="text-muted-foreground text-xs">{probe.reason}</span>}
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
