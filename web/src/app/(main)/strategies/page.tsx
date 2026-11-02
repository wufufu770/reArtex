"use client";

import * as React from "react";

import Link from "next/link";

import { LibraryBigIcon, PinIcon, RefreshCwIcon, SearchIcon } from "lucide-react";
import { toast } from "sonner";

import { StatusBadge } from "@/components/status-badge";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { api } from "@/lib/api";
import type { Strategy, StrategyState } from "@/lib/types";

import { fmtDateTime, fmtScore, STRATEGY_STATES } from "./_components/strategy-format";
import { StrategyNav } from "./_components/strategy-nav";
import { UtilityMeter } from "./_components/utility-meter";

// ALL_STATES 表示「不按状态筛」——后端不传 state 就是全部。
const ALL_STATES = "__all__";

// 策略库变动很慢（人工裁决 + curator 后台动作），15 秒一轮足够跟上。
const POLL_MS = 15_000;

function StrategyRow({ strategy }: { strategy: Strategy }) {
  return (
    <TableRow>
      <TableCell>
        <Link
          href={`/strategies/detail?id=${strategy.id}`}
          className="flex flex-col gap-0.5 font-medium hover:text-primary"
        >
          <span className="truncate">{strategy.name}</span>
          <span className="truncate font-normal text-muted-foreground text-xs">{strategy.summary || "（无摘要）"}</span>
        </Link>
      </TableCell>
      <TableCell>
        <div className="flex flex-col items-start gap-1">
          <StatusBadge domain="strategy" value={strategy.state} dot />
          {strategy.pinned && (
            <Badge variant="outline" className="gap-1 px-1.5 py-0 text-[10px]">
              <PinIcon className="size-3" />
              唯一证人
            </Badge>
          )}
        </div>
      </TableCell>
      <TableCell>
        <div className="flex w-28 flex-col gap-1">
          <UtilityMeter value={strategy.utility} />
          <span className="text-muted-foreground text-xs tabular-nums">{fmtScore(strategy.utility)}</span>
        </div>
      </TableCell>
      <TableCell>
        <span className="text-muted-foreground text-xs tabular-nums">v{strategy.version}</span>
      </TableCell>
      <TableCell>
        <div className="flex flex-col gap-0.5 text-xs tabular-nums">
          <span>
            用 {strategy.usage_count} · 命中 {strategy.hit_count} · 未命中 {strategy.miss_count}
          </span>
          <span className="text-muted-foreground">置信 {fmtScore(strategy.confidence)}</span>
        </div>
      </TableCell>
      <TableCell>
        <div className="flex flex-col items-start gap-0.5">
          <span className="font-mono text-muted-foreground text-xs">#{strategy.id}</span>
          <span className="text-muted-foreground text-xs">{fmtDateTime(strategy.updated_at)}</span>
        </div>
      </TableCell>
    </TableRow>
  );
}

function StrategyTable({ rows, loaded }: { rows: Strategy[]; loaded: boolean }) {
  if (!loaded) {
    return <p className="py-6 text-center text-muted-foreground text-sm">加载中…</p>;
  }
  if (rows.length === 0) {
    return (
      <Empty className="mx-4 border border-dashed lg:mx-6">
        <EmptyHeader>
          <EmptyTitle>策略库还是空的</EmptyTitle>
          <EmptyDescription>
            策略由 agent 提议、宿主裁决产生：新策略一律先进隔离态，晋级要过 curator 的三条件。当前没有任何策略 ——
            先去「待裁决提议」页看看有没有等着批的。
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
          <TableHead>效用</TableHead>
          <TableHead>版本</TableHead>
          <TableHead className="hidden md:table-cell">观测</TableHead>
          <TableHead>更新</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((s) => (
          <StrategyRow key={s.id} strategy={s} />
        ))}
      </TableBody>
    </Table>
  );
}

export default function StrategiesPage() {
  const [state, setState] = React.useState<string>(ALL_STATES);
  const [query, setQuery] = React.useState("");
  const [rows, setRows] = React.useState<Strategy[]>([]);
  const [loaded, setLoaded] = React.useState(false);
  const [refreshing, setRefreshing] = React.useState(false);

  // 内容没变就不 setState，避免整页重渲染（与任务页同一套做法）。
  const lastRef = React.useRef<string>("");
  // 首屏失败要报一次，之后的轮询失败静默（下一轮自动重试）。
  const reportedRef = React.useRef(false);

  const load = React.useCallback(async () => {
    try {
      const list = await api.strategies(state === ALL_STATES ? undefined : (state as StrategyState));
      const sig = JSON.stringify(list);
      if (sig === lastRef.current) return;
      lastRef.current = sig;
      setRows(list);
      reportedRef.current = false;
    } catch (e) {
      if (!reportedRef.current) {
        reportedRef.current = true;
        toast.error(`读取策略库失败：${(e as Error).message}`);
      }
    } finally {
      setLoaded(true);
    }
  }, [state]);

  React.useEffect(() => {
    void load();
    const t = setInterval(() => void load(), POLL_MS);
    return () => clearInterval(t);
  }, [load]);

  // 刷新的是 utility 的**权威物化视图**；列表里的 utility 列是缓存值，由 curator 维护。
  async function refreshUtility() {
    setRefreshing(true);
    try {
      await api.refreshStrategyUtility();
      lastRef.current = "";
      await load();
      toast.success("utility 物化视图已刷新");
    } catch (e) {
      toast.error(`刷新失败：${(e as Error).message}`);
    } finally {
      setRefreshing(false);
    }
  }

  const keyword = query.trim().toLowerCase();
  const visible = keyword
    ? rows.filter(
        (s) =>
          s.name.toLowerCase().includes(keyword) ||
          s.summary.toLowerCase().includes(keyword) ||
          String(s.id) === keyword,
      )
    : rows;

  const activeCount = rows.filter((s) => s.state === "active").length;
  const quarantinedCount = rows.filter((s) => s.state === "quarantined").length;
  const pinnedCount = rows.filter((s) => s.pinned).length;

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div className="flex items-start justify-between gap-3">
        <div>
          <h1 className="font-semibold text-xl tracking-tight">策略库</h1>
          <p className="text-muted-foreground text-sm">
            L4 知识图谱 · 一条策略 = 一份「怎么打这类目标」的参考文本 · 正文不会自动执行
          </p>
        </div>
        <Button variant="outline" size="sm" onClick={() => void refreshUtility()} disabled={refreshing}>
          {refreshing ? <Spinner data-icon="inline-start" /> : <RefreshCwIcon />}
          刷新 utility
        </Button>
      </div>

      <StrategyNav current="strategies" />

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">策略</p>
            <p className="font-semibold text-lg tabular-nums">{rows.length}</p>
          </CardContent>
        </Card>
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">在役</p>
            <p className="font-semibold text-lg tabular-nums">{activeCount}</p>
          </CardContent>
        </Card>
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">隔离中</p>
            <p className="font-semibold text-lg tabular-nums">{quarantinedCount}</p>
          </CardContent>
        </Card>
        <Card size="sm" className="gap-1">
          <CardContent>
            <p className="text-muted-foreground text-xs">已 pin</p>
            <p className="font-semibold text-lg tabular-nums">{pinnedCount}</p>
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>策略列表</CardTitle>
          <CardDescription>
            共 {rows.length} 条{visible.length !== rows.length ? ` · 命中 ${visible.length} 条` : ""} ·
            点击进入可看正文、证据链与裁决
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          <div className="flex flex-wrap items-center gap-2">
            <div className="relative w-full sm:max-w-xs">
              <SearchIcon className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
              <Input
                placeholder="搜索名称 / 摘要 / id"
                className="pl-8"
                value={query}
                onChange={(e) => setQuery(e.target.value)}
              />
            </div>
            <Select value={state} onValueChange={setState}>
              <SelectTrigger className="w-full sm:w-56">
                <SelectValue placeholder="状态" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={ALL_STATES}>全部状态</SelectItem>
                {STRATEGY_STATES.map((s) => (
                  <SelectItem key={s.value} value={s.value}>
                    {s.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <StrategyTable rows={visible} loaded={loaded} />
        </CardContent>
      </Card>

      <p className="text-muted-foreground text-xs">
        <LibraryBigIcon className="mr-1 inline size-3.5 align-text-bottom" />
        效用列读的是 strategies 表上的缓存值，权威 utility 在 strategy_utility 物化视图里；「刷新
        utility」刷的是那张视图。
      </p>
    </div>
  );
}
