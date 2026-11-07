"use client";

// v4.1 auditor 建议队列卡（方案 §3 建议循环）：auditor 裁决时观察到同类动作反复出现，
// 会建议把它沉淀成确定性拦截种类（采纳后这类调用连 auditor 都不用再问）。这里只做
// 人工裁决：采纳（后端落成种类行）或忽略；operator 全队列共享一个输入。
// v6.1：卡头加 待裁决/已采纳/已忽略 三个过滤页签——已采纳页能看到采纳落成的回链种类
// 还在不在（按 suggestion_id 反查种类表），并可撤销采纳（删种类、建议放回待裁决）；
// 已忽略页只作历史留痕，不提供恢复入口。

import * as React from "react";

import { LightbulbIcon, RefreshCwIcon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import type { SodCategory, SodCategorySuggestion } from "@/lib/types";

// ACTION_LABEL 与后端 sodpolicy.CategoryAction* 对应（建议里的 action 同一词汇）。
const ACTION_LABEL: Record<string, string> = {
  hard_deny: "硬拒",
  ask: "转人工",
  auditor: "审计裁决",
  pass: "放行",
};

type SuggestionStatus = SodCategorySuggestion["status"];

// TABS 是卡头的三个过滤页签：切换即按对应 status 拉取，默认看待裁决。
const TABS: { value: SuggestionStatus; label: string }[] = [
  { value: "pending", label: "待裁决" },
  { value: "adopted", label: "已采纳" },
  { value: "dismissed", label: "已忽略" },
];

// EMPTY_TEXT 是各页签的空态文案。
const EMPTY_TEXT: Record<SuggestionStatus, string> = {
  pending: "暂无待裁决建议。auditor 在裁决中发现「同类动作反复出现」时才会提议新种类。",
  adopted: "还没有已采纳的建议——去「待裁决」页采纳一条，落成的种类会回链到这里。",
  dismissed: "还没有忽略记录。",
};

// fmtTime 把时间戳压成 MM-DD HH:mm（忽略记录按时间定位就够用了）。
function fmtTime(iso?: string | null): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toLocaleString("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" });
}

// catBadge 把采纳回链种类的存在状态压成一个徽标：按 suggestion_id 反查种类表——
// 查到且 enabled=在用 / 查到但未 enabled=已停用 / 查不到=已删除（比如被手动删了）。
function catBadge(cat: SodCategory | undefined): {
  label: string;
  variant: "default" | "outline" | "destructive";
} {
  if (!cat) return { label: "对应种类：已删除", variant: "destructive" };
  if (!cat.enabled) return { label: "对应种类：已停用", variant: "outline" };
  return { label: "对应种类：在用", variant: "default" };
}

export function SodSuggestionCard() {
  const [tab, setTab] = React.useState<SuggestionStatus>("pending");
  const [items, setItems] = React.useState<SodCategorySuggestion[]>([]);
  const [categories, setCategories] = React.useState<SodCategory[]>([]);
  const [loaded, setLoaded] = React.useState(false);
  const [operator, setOperator] = React.useState("sir");
  const [busyId, setBusyId] = React.useState(0);

  const load = React.useCallback(async () => {
    try {
      // 已采纳页要按 suggestion_id 反查种类表，顺便把种类列表拉上；其它页签用不到。
      const [suggestions, cats] = await Promise.all([
        api.sodCategorySuggestions(tab),
        tab === "adopted" ? api.sodCategories().then((r) => r.categories) : Promise.resolve<SodCategory[]>([]),
      ]);
      setItems(suggestions);
      setCategories(cats);
      setLoaded(true);
    } catch (e) {
      toast.error(`读取建议队列失败：${(e as Error).message}`);
    }
  }, [tab]);

  React.useEffect(() => {
    setLoaded(false);
    void load();
  }, [load]);

  async function decide(row: SodCategorySuggestion, decision: "adopted" | "dismissed") {
    if (!operator.trim()) {
      toast.error("先填操作者身份（审计留痕）");
      return;
    }
    setBusyId(row.id);
    try {
      await api.decideSodCategorySuggestion(row.id, decision, operator.trim());
      toast.success(decision === "adopted" ? "已沉淀为拦截种类" : "已忽略该建议");
      await load();
    } catch (e) {
      toast.error(`裁决失败：${(e as Error).message}`);
    } finally {
      setBusyId(0);
    }
  }

  async function revoke(row: SodCategorySuggestion) {
    if (!operator.trim()) {
      toast.error("先填操作者身份（审计留痕）");
      return;
    }
    if (!window.confirm(`撤销采纳「${row.name}」？将删除对应种类并把建议放回待裁决。`)) return;
    setBusyId(row.id);
    try {
      const r = await api.revokeSodCategorySuggestion(row.id, operator.trim());
      toast.success(r.removed_category ? "已撤销：对应种类已删除" : "该种类已不存在，建议已放回");
      await load();
    } catch (e) {
      toast.error(`撤销失败：${(e as Error).message}`);
    } finally {
      setBusyId(0);
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <LightbulbIcon className="size-4" /> auditor 建议队列
        </CardTitle>
        <CardDescription>
          auditor 的裁量经验在这里沉淀为确定性规则——采纳后这类调用走种类表，连 auditor 都不用再问。
        </CardDescription>
        <div className="flex w-fit items-center gap-0.5 rounded-md border bg-muted/40 p-0.5">
          {TABS.map((t) => (
            <Button
              key={t.value}
              size="sm"
              variant={tab === t.value ? "secondary" : "ghost"}
              className="h-6 px-2.5 text-xs"
              onClick={() => setTab(t.value)}
            >
              {t.label}
            </Button>
          ))}
        </div>
      </CardHeader>
      <CardContent className="grid gap-3">
        <div className="flex flex-wrap items-end justify-between gap-2">
          <div className="grid gap-1.5">
            <Label htmlFor="sod-suggestion-operator">操作者</Label>
            <Input
              id="sod-suggestion-operator"
              className="w-28"
              value={operator}
              onChange={(e) => setOperator(e.target.value)}
            />
          </div>
          <Button size="sm" variant="ghost" className="h-7 text-xs" onClick={() => void load()}>
            <RefreshCwIcon /> 刷新
          </Button>
        </div>
        {!loaded && <p className="py-4 text-center text-muted-foreground text-sm">加载中…</p>}
        {loaded && items.length === 0 && (
          <p className="py-4 text-center text-muted-foreground text-xs">{EMPTY_TEXT[tab]}</p>
        )}
        {loaded && items.length > 0 && (
          <div className="grid max-h-[60svh] gap-2 overflow-y-auto pr-1">
            {items.map((row) => {
              // 回链种类只在已采纳页有意义（其它页签 categories 恒为空，反查必不中）。
              const cat = categories.find((c) => c.suggestion_id === row.id);
              const cb = catBadge(cat);
              return (
                <div key={row.id} className="flex flex-wrap items-center gap-2 rounded-md border p-2">
                  <Badge variant="outline" className="text-[10px]">
                    {ACTION_LABEL[row.action] ?? row.action}
                  </Badge>
                  <span className="font-medium text-sm">{row.name}</span>
                  <code
                    className="min-w-0 flex-1 truncate rounded bg-muted px-1.5 py-0.5 font-mono text-xs"
                    title={row.pattern}
                  >
                    {row.pattern || "—"}
                  </code>
                  {tab === "pending" && (
                    <Badge variant="secondary" className="text-[10px]">
                      证据 {row.evidence?.length ?? 0} 条
                    </Badge>
                  )}
                  {tab === "adopted" && (
                    <Badge
                      variant={cb.variant}
                      className="text-[10px]"
                      title={cat ? `种类「${cat.name}」` : "采纳落成的种类已被删除"}
                    >
                      {cb.label}
                    </Badge>
                  )}
                  {tab === "dismissed" && (
                    <span className="whitespace-nowrap text-muted-foreground text-xs">
                      {row.decided_by || "—"} · {fmtTime(row.decided_at)}
                    </span>
                  )}
                  <span className="min-w-0 basis-full truncate text-muted-foreground text-[11px]" title={row.reason}>
                    {row.reason || "—"}
                  </span>
                  {tab === "pending" && (
                    <div className="flex items-center gap-1.5">
                      <Button
                        size="sm"
                        className="h-7 px-2 text-xs"
                        disabled={busyId === row.id}
                        onClick={() => void decide(row, "adopted")}
                      >
                        采纳
                      </Button>
                      <Button
                        size="sm"
                        variant="outline"
                        className="h-7 px-2 text-xs"
                        disabled={busyId === row.id}
                        onClick={() => void decide(row, "dismissed")}
                      >
                        忽略
                      </Button>
                    </div>
                  )}
                  {tab === "adopted" && (
                    <Button
                      size="sm"
                      variant="destructive"
                      className="h-7 px-2 text-xs"
                      disabled={busyId === row.id}
                      title="删除采纳落成的种类，并把建议放回待裁决"
                      onClick={() => void revoke(row)}
                    >
                      撤销采纳
                    </Button>
                  )}
                </div>
              );
            })}
          </div>
        )}
      </CardContent>
    </Card>
  );
}
