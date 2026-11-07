"use client";

// v4.1 影子对照卡（方案 §11-G1）：shadow 模式下新旧链并跑——最终动作永远按旧链，
// 新链（种类表 + auditor）的「假如 enforce 会怎么判」只记录差异到对照表。
// 切 enforce 前先在这里核对：没有系统性误伤，才值得接管。

import * as React from "react";

import { ArrowRightIcon, GitCompareArrowsIcon, RefreshCwIcon } from "lucide-react";
import { toast } from "sonner";

import { StatusBadge } from "@/components/status-badge";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { api } from "@/lib/api";
import type { ShadowDiff, SodMergedGateMode } from "@/lib/types";

// MODE_META 是合并闸三态的徽标文案与配色。
const MODE_META: Record<SodMergedGateMode, { label: string; variant: "default" | "secondary" | "outline" }> = {
  off: { label: "off · 旧链裁决", variant: "secondary" },
  shadow: { label: "shadow · 影子对照中", variant: "outline" },
  enforce: { label: "enforce · 新链已接管", variant: "default" },
};

// MODE_DESCRIPTION 是卡头描述随 mode 联动的文案：off 没在对照、shadow 才有新差异落表、
// enforce 后新链已接管，这张表只剩 shadow 期的历史存档。
const MODE_DESCRIPTION: Record<SodMergedGateMode, string> = {
  off: "合并闸关闭，新旧链对照未启用。",
  shadow:
    "shadow 模式新旧链并跑：最终动作永远按旧链，新链的裁决只记录差异（最多 100 条）。「不一致」的行是新旧链的分歧——切 enforce 前先核对有没有系统性误伤。",
  enforce: "新链已接管裁决——本卡为 shadow 期的历史对照存档（差异不再新增）。",
};

// fmtTime 把时间戳压成 MM-DD HH:mm（对照行按时间定位就够用了）。
function fmtTime(iso?: string | null): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toLocaleString("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" });
}

export function SodShadowCard() {
  const [mode, setMode] = React.useState<SodMergedGateMode>("off");
  const [diffs, setDiffs] = React.useState<ShadowDiff[]>([]);
  const [loaded, setLoaded] = React.useState(false);

  const load = React.useCallback(async () => {
    try {
      const r = await api.sodShadowDiffs(100);
      setDiffs(r.diffs);
      setMode(r.mode);
      setLoaded(true);
    } catch (e) {
      toast.error(`读取影子对照失败：${(e as Error).message}`);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  // 只有 shadow 模式会有新对照落表，其它模式静置即可。
  React.useEffect(() => {
    if (mode !== "shadow") return;
    const t = setInterval(() => void load(), 30_000);
    return () => clearInterval(t);
  }, [mode, load]);

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <GitCompareArrowsIcon className="size-4" /> 影子对照
          <Badge variant={MODE_META[mode].variant} className="text-[10px]">
            {MODE_META[mode].label}
          </Badge>
        </CardTitle>
        <CardDescription>{MODE_DESCRIPTION[mode]}</CardDescription>
      </CardHeader>
      <CardContent className="grid gap-2">
        <div className="flex items-center justify-end">
          <Button size="sm" variant="ghost" className="h-7 text-xs" onClick={() => void load()}>
            <RefreshCwIcon /> 刷新
          </Button>
        </div>
        {!loaded && <p className="py-4 text-center text-muted-foreground text-sm">加载中…</p>}
        {loaded && diffs.length === 0 && (
          <p className="py-4 text-center text-muted-foreground text-xs">
            影子模式开启后，新旧链的裁决差异会出现在这里。
          </p>
        )}
        {loaded && diffs.length > 0 && (
          <div className="overflow-x-auto rounded-md border">
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead className="w-[96px]">时间</TableHead>
                  <TableHead>工具</TableHead>
                  <TableHead className="w-[220px]">旧链 → 新链</TableHead>
                  <TableHead>原因对比</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {diffs.map((d) => {
                  const same = d.old_verdict === d.new_verdict;
                  return (
                    <TableRow key={d.id}>
                      <TableCell className="text-muted-foreground text-xs tabular-nums">
                        {fmtTime(d.created_at)}
                      </TableCell>
                      <TableCell>
                        <span className="font-mono text-xs">{d.tool_name || "—"}</span>
                        {d.task_id ? (
                          <span className="ml-1.5 text-[10px] text-muted-foreground">任务 {d.task_id}</span>
                        ) : null}
                      </TableCell>
                      <TableCell>
                        <div className="flex flex-wrap items-center gap-1">
                          <StatusBadge domain="sodVerdict" value={d.old_verdict} />
                          <ArrowRightIcon className="size-3 text-muted-foreground" />
                          <StatusBadge domain="sodVerdict" value={d.new_verdict} />
                          <Badge
                            variant={same ? "secondary" : "outline"}
                            className={same ? "text-[10px]" : "border-amber-400 text-[10px] text-amber-600"}
                          >
                            {same ? "一致" : "不一致"}
                          </Badge>
                        </div>
                      </TableCell>
                      <TableCell>
                        <div className="flex flex-col gap-0.5">
                          <span className="truncate font-mono text-[11px] text-muted-foreground" title={d.old_reason}>
                            旧：{d.old_reason || "—"}
                          </span>
                          <span className="truncate font-mono text-[11px]" title={d.new_reason}>
                            新：{d.new_reason || "—"}
                          </span>
                        </div>
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          </div>
        )}
      </CardContent>
    </Card>
  );
}
