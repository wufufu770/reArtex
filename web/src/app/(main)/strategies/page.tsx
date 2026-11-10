"use client";

import * as React from "react";

import Link from "next/link";
import { useRouter } from "next/navigation";

import { LibraryBigIcon, PencilIcon, PinIcon, RefreshCwIcon, SearchIcon, SparklesIcon } from "lucide-react";
import { toast } from "sonner";

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
import { Spinner } from "@/components/ui/spinner";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type { Strategy, StrategyState } from "@/lib/types";

import { fmtDateTime, fmtScore, STRATEGY_STATES } from "./_components/strategy-format";
import { StrategyNav } from "./_components/strategy-nav";
import { UtilityMeter } from "./_components/utility-meter";

// ALL_STATES 表示「不按状态筛」——后端不传 state 就是全部。
const ALL_STATES = "__all__";

// 策略库变动很慢（人工裁决 + curator 后台动作），15 秒一轮足够跟上。
const POLL_MS = 15_000;

function StrategyRow({ strategy, onEdit }: { strategy: Strategy; onEdit: (strategy: Strategy) => void }) {
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
        <div className="flex flex-col gap-0.5">
          <span className="font-mono text-muted-foreground text-xs">#{strategy.id}</span>
          <span className="text-muted-foreground text-xs">{fmtDateTime(strategy.updated_at)}</span>
        </div>
      </TableCell>
      <TableCell>
        <Button size="sm" variant="ghost" onClick={() => onEdit(strategy)} title="编辑名称与摘要（版本化保存）">
          <PencilIcon data-icon="inline-start" />
          编辑
        </Button>
      </TableCell>
    </TableRow>
  );
}

function StrategyTable({
  rows,
  loaded,
  onEdit,
}: {
  rows: Strategy[];
  loaded: boolean;
  onEdit: (strategy: Strategy) => void;
}) {
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
          <TableHead>操作</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((s) => (
          <StrategyRow key={s.id} strategy={s} onEdit={onEdit} />
        ))}
      </TableBody>
    </Table>
  );
}

export default function StrategiesPage() {
  const router = useRouter();
  const [state, setState] = React.useState<string>(ALL_STATES);
  const [query, setQuery] = React.useState("");
  const [rows, setRows] = React.useState<Strategy[]>([]);
  const [loaded, setLoaded] = React.useState(false);
  const [refreshing, setRefreshing] = React.useState(false);

  // 摄入学习弹窗：url 与粘贴文本二选一（后端优先抓 url）。
  const [ingestOpen, setIngestOpen] = React.useState(false);
  const [ingestUrl, setIngestUrl] = React.useState("");
  const [ingestText, setIngestText] = React.useState("");
  const [ingesting, setIngesting] = React.useState(false);

  // 行内「编辑」弹窗：改 name/summary（版本化写，后端 version+1）。
  const [editTarget, setEditTarget] = React.useState<Strategy | null>(null);
  const [editName, setEditName] = React.useState("");
  const [editSummary, setEditSummary] = React.useState("");
  const [savingEdit, setSavingEdit] = React.useState(false);

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

  // 摄入学习：url / 文本二选一交给后端（url 优先）。候选不直接入库，
  // 全进待裁决提议队列 —— 成功后弹带跳转按钮的 toast。
  async function submitIngest() {
    const url = ingestUrl.trim();
    const text = ingestText.trim();
    if (!url && !text) return;
    setIngesting(true);
    try {
      const r = await api.strategyIngest({ url: url || undefined, text: text || undefined });
      if (r.proposals > 0) {
        toast.success(`学到 ${r.proposals} 条候选策略`, {
          description: "候选先进待裁决队列，裁决通过后才会进入策略库",
          action: {
            label: "去裁决",
            onClick: () => router.push("/strategies/proposals"),
          },
        });
        setIngestOpen(false);
        setIngestUrl("");
        setIngestText("");
      } else {
        toast.warning(r.note || "模型没有提炼出候选策略，可换一份更具体的素材再试");
      }
    } catch (e) {
      toast.error(`摄入失败：${(e as Error).message}`);
    } finally {
      setIngesting(false);
    }
  }

  function openEdit(strategy: Strategy) {
    setEditTarget(strategy);
    setEditName(strategy.name);
    setEditSummary(strategy.summary);
  }

  // 行内编辑只写 name/summary；正文六元组在详情页的正文 Tab 编辑。
  async function saveEdit() {
    if (!editTarget) return;
    const name = editName.trim();
    if (!name) {
      toast.error("策略名不能为空");
      return;
    }
    setSavingEdit(true);
    try {
      const r = await api.strategyEdit(editTarget.id, { name, summary: editSummary });
      toast.success(`已保存为 v${r.version}`);
      setEditTarget(null);
      lastRef.current = "";
      await load();
    } catch (e) {
      toast.error(`保存失败：${(e as Error).message}`);
    } finally {
      setSavingEdit(false);
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
        <div className="flex items-center gap-2">
          <Button variant="outline" size="sm" onClick={() => setIngestOpen(true)}>
            <SparklesIcon />
            摄入学习
          </Button>
          <Button variant="outline" size="sm" onClick={() => void refreshUtility()} disabled={refreshing}>
            {refreshing ? <Spinner data-icon="inline-start" /> : <RefreshCwIcon />}
            刷新 utility
          </Button>
        </div>
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
          <StrategyTable rows={visible} loaded={loaded} onEdit={openEdit} />
        </CardContent>
      </Card>

      <p className="text-muted-foreground text-xs">
        <LibraryBigIcon className="mr-1 inline size-3.5 align-text-bottom" />
        效用列读的是 strategies 表上的缓存值，权威 utility 在 strategy_utility 物化视图里；「刷新
        utility」刷的是那张视图。
      </p>

      {/* 摄入学习：报告/网址 → LLM 总结 → 候选策略提议（不直接入库） */}
      <Dialog open={ingestOpen} onOpenChange={setIngestOpen}>
        <DialogContent className="sm:max-w-xl">
          <DialogHeader>
            <DialogTitle>摄入学习</DialogTitle>
            <DialogDescription>
              给一份素材（网址或粘贴的文本，二选一），由 LLM 总结提炼成候选策略 ——
              先进「待裁决提议」队列，裁决通过才会进入策略库。
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-4 py-2">
            <div className="grid gap-1.5">
              <Label htmlFor="ingest-url">素材网址</Label>
              <Input
                id="ingest-url"
                placeholder="https://example.com/report"
                value={ingestUrl}
                onChange={(e) => setIngestUrl(e.target.value)}
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="ingest-text">或粘贴文本</Label>
              <Textarea
                id="ingest-text"
                className="min-h-40"
                placeholder="粘贴报告 / 复盘 / 文章正文（太短的素材学不出东西）"
                value={ingestText}
                onChange={(e) => setIngestText(e.target.value)}
              />
            </div>
            <p className="text-muted-foreground text-xs">两个都填时优先抓取网址；学习会调用 LLM，可能要等一两分钟。</p>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setIngestOpen(false)} disabled={ingesting}>
              取消
            </Button>
            <Button
              onClick={() => void submitIngest()}
              disabled={ingesting || (!ingestUrl.trim() && !ingestText.trim())}
            >
              {ingesting && <Spinner data-icon="inline-start" />}
              {ingesting ? "学习中…" : "开始学习"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* 行内编辑：改策略名与摘要（版本化写） */}
      <Dialog
        open={editTarget !== null}
        onOpenChange={(open) => {
          if (!open) setEditTarget(null);
        }}
      >
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>编辑策略 #{editTarget?.id}</DialogTitle>
            <DialogDescription>
              保存为一次版本化编辑（version+1）；正文六元组请到详情页的「正文」Tab 改。
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-4 py-2">
            <div className="grid gap-1.5">
              <Label htmlFor="strategy-edit-name">策略名</Label>
              <Input
                id="strategy-edit-name"
                value={editName}
                onChange={(e) => setEditName(e.target.value)}
                placeholder="策略名不能为空"
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="strategy-edit-summary">摘要</Label>
              <Textarea
                id="strategy-edit-summary"
                className="min-h-24"
                value={editSummary}
                onChange={(e) => setEditSummary(e.target.value)}
                placeholder="一句话说明这条策略打什么、怎么打"
              />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setEditTarget(null)} disabled={savingEdit}>
              取消
            </Button>
            <Button onClick={() => void saveEdit()} disabled={savingEdit || !editName.trim()}>
              {savingEdit && <Spinner data-icon="inline-start" />}
              {savingEdit ? "保存中…" : "保存"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
