"use client";

// v4.1 auditor 建议队列卡（方案 §3 建议循环）：auditor 裁决时观察到同类动作反复出现，
// 会建议把它沉淀成确定性拦截种类（采纳后这类调用连 auditor 都不用再问）。这里只做
// 人工裁决：采纳（后端落成种类行）或忽略；operator 全队列共享一个输入。

import * as React from "react";

import { LightbulbIcon, RefreshCwIcon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import type { SodCategorySuggestion } from "@/lib/types";

// ACTION_LABEL 与后端 sodpolicy.CategoryAction* 对应（建议里的 action 同一词汇）。
const ACTION_LABEL: Record<string, string> = {
  hard_deny: "硬拒",
  ask: "转人工",
  auditor: "审计裁决",
  pass: "放行",
};

export function SodSuggestionCard() {
  const [items, setItems] = React.useState<SodCategorySuggestion[]>([]);
  const [loaded, setLoaded] = React.useState(false);
  const [operator, setOperator] = React.useState("sir");
  const [busyId, setBusyId] = React.useState(0);

  const load = React.useCallback(async () => {
    try {
      setItems(await api.sodCategorySuggestions("pending"));
      setLoaded(true);
    } catch (e) {
      toast.error(`读取建议队列失败：${(e as Error).message}`);
    }
  }, []);

  React.useEffect(() => {
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

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <LightbulbIcon className="size-4" /> auditor 建议队列
        </CardTitle>
        <CardDescription>
          auditor 的裁量经验在这里沉淀为确定性规则——采纳后这类调用走种类表，连 auditor 都不用再问。
        </CardDescription>
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
          <p className="py-4 text-center text-muted-foreground text-xs">
            暂无待裁决建议。auditor 在裁决中发现「同类动作反复出现」时才会提议新种类。
          </p>
        )}
        {loaded && items.length > 0 && (
          <div className="grid gap-2">
            {items.map((row) => (
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
                <Badge variant="secondary" className="text-[10px]">
                  证据 {row.evidence?.length ?? 0} 条
                </Badge>
                <span className="min-w-0 flex-1 basis-full truncate text-muted-foreground text-xs" title={row.reason}>
                  {row.reason || "—"}
                </span>
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
              </div>
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  );
}
