"use client";

// 传承图页（P5 第三张图）：跨任务经验的列表 / 指纹 / 人工评审。
// 评审是人在环路的确定性闸：draft → accepted（进注入池）/ retired。无自动晋升。

import * as React from "react";

import Link from "next/link";

import { CheckIcon, ChevronDownIcon, ChevronUpIcon, HistoryIcon, LibraryBigIcon, XIcon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api } from "@/lib/api";
import type { LegacyNode } from "@/lib/types";

const KIND_LABELS: Record<string, string> = {
  experience: "经验",
  pitfall: "弯路",
  fingerprint_summary: "画像",
};

const STATE_TABS = [
  { value: "", label: "全部" },
  { value: "draft", label: "待评审" },
  { value: "accepted", label: "已接受" },
  { value: "retired", label: "已退役" },
];

// STATE_META 是状态徽章的展示映射（查表替代嵌套三元，D2 撤销/删除按钮也按态分发）。
const STATE_META: Record<string, { label: string; className: string }> = {
  draft: { label: "待评审", className: "border-amber-500/40 text-amber-600" },
  accepted: { label: "已接受", className: "border-emerald-500/40 text-emerald-600" },
  retired: { label: "已退役", className: "text-muted-foreground" },
};

const REVIEW_TOAST: Record<string, string> = {
  accepted: "已接受：进入注入池",
  draft: "已恢复为待评审",
  retired: "已退役",
};

export default function LegacyPage() {
  const [nodes, setNodes] = React.useState<LegacyNode[]>([]);
  const [state, setState] = React.useState("");
  const [loaded, setLoaded] = React.useState(false);
  const [busyId, setBusyId] = React.useState<number | null>(null);
  // 展开的 detail 正文（节点 id 集合）：蒸馏正文（detail）默认收起为 6 行摘要，
  // 点「展开」看全文——用户拍板「策略的要能展开」。
  const [expanded, setExpanded] = React.useState<Set<number>>(() => new Set());

  const load = React.useCallback(async () => {
    try {
      setNodes(await api.legacyNodes(state));
    } catch (e) {
      toast.error(`读取传承图失败：${(e as Error).message}`);
    } finally {
      setLoaded(true);
    }
  }, [state]);

  React.useEffect(() => {
    setLoaded(false);
    void load();
  }, [load]);

  async function review(node: LegacyNode, next: "accepted" | "retired" | "draft") {
    setBusyId(node.id);
    try {
      await api.reviewLegacyNode(node.id, next);
      toast.success(REVIEW_TOAST[next] ?? "已更新");
      await load();
    } catch (e) {
      toast.error(`评审失败：${(e as Error).message}`);
    } finally {
      setBusyId(null);
    }
  }

  async function remove(node: LegacyNode) {
    if (!window.confirm(`删除经验 #${node.id}「${node.title}」？删除不可恢复（审计留快照）。`)) return;
    setBusyId(node.id);
    try {
      await api.deleteLegacyNode(node.id);
      toast.success("已删除（审计快照已留痕）");
      await load();
    } catch (e) {
      toast.error(`删除失败：${(e as Error).message}`);
    } finally {
      setBusyId(null);
    }
  }

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div className="flex items-start justify-between gap-3">
        <div>
          <h1 className="font-semibold text-xl tracking-tight">传承图</h1>
          <p className="text-muted-foreground text-sm">
            跨任务经验：任务终局自动蒸馏出草稿，经你评审进入注入池——后续同族任务规划时自动携带。
          </p>
        </div>
        <Tabs value={state} onValueChange={setState}>
          <TabsList>
            {STATE_TABS.map((tab) => (
              <TabsTrigger key={tab.value} value={tab.value}>
                {tab.label}
              </TabsTrigger>
            ))}
          </TabsList>
        </Tabs>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>经验列表</CardTitle>
          <CardDescription>draft 只有不进注入池；accepted 才会在新同族任务规划时被携带。</CardDescription>
        </CardHeader>
        <CardContent className="grid gap-3">
          {!loaded && <p className="py-6 text-center text-muted-foreground text-sm">加载中…</p>}
          {loaded && nodes.length === 0 && (
            <Empty className="border border-dashed">
              <EmptyHeader>
                <EmptyTitle>还没有经验沉淀</EmptyTitle>
                <EmptyDescription>任务完成（done）时系统会自动蒸馏交接链与结论到这里，等你评审。</EmptyDescription>
              </EmptyHeader>
            </Empty>
          )}
          {nodes.map((node) => {
            // 指纹徽章区（D3）：空指纹 / 值全空串都不渲染，避免空块。
            const fingerprintEntries = Object.entries(node.fingerprint ?? {}).filter(
              ([k, v]) => k.trim() !== "" && String(v).trim() !== "",
            );
            const summary = (node.summary ?? "").trim();
            const detail = (node.detail ?? "").trim();
            const open = expanded.has(node.id);
            return (
              <div key={node.id} className="grid gap-2 rounded-lg border p-3">
                <div className="flex min-w-0 flex-wrap items-center gap-2">
                  <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
                    {KIND_LABELS[node.kind] ?? node.kind}
                  </Badge>
                  <Badge
                    variant="outline"
                    className={`px-1.5 py-0 text-[10px] ${STATE_META[node.state]?.className ?? ""}`}
                  >
                    {STATE_META[node.state]?.label ?? node.state}
                  </Badge>
                  <span className="min-w-0 flex-1 truncate font-medium text-sm">{node.title}</span>
                  <span className="shrink-0 font-mono text-[10px] text-muted-foreground">#{node.id}</span>
                  {node.source_task && (
                    <span className="shrink-0 text-[10px] text-muted-foreground">来自任务 #{node.source_task}</span>
                  )}
                </div>
                {summary !== "" && summary !== "[]" && (
                  <p className="whitespace-pre-wrap text-muted-foreground text-xs">{summary}</p>
                )}
                {/* 蒸馏正文（detail）：默认收起 6 行，展开看全文（v0.9.17 用户拍板） */}
                {detail !== "" && detail !== summary && (
                  <div className="grid gap-1">
                    <p className={`whitespace-pre-wrap text-xs ${open ? "" : "line-clamp-6"}`}>{detail}</p>
                    <Button
                      size="sm"
                      variant="ghost"
                      className="h-6 w-fit px-2 text-[11px]"
                      onClick={() =>
                        setExpanded((prev) => {
                          const next = new Set(prev);
                          if (next.has(node.id)) next.delete(node.id);
                          else next.add(node.id);
                          return next;
                        })
                      }
                    >
                      {open ? <ChevronUpIcon /> : <ChevronDownIcon />}
                      {open ? "收起正文" : "展开正文"}
                    </Button>
                  </div>
                )}
                {fingerprintEntries.length > 0 && (
                  <div className="flex flex-wrap gap-1">
                    {fingerprintEntries.map(([k, v]) => (
                      <Badge key={k} variant="outline" className="px-1.5 py-0 font-mono text-[10px]">
                        {k}={String(v)}
                      </Badge>
                    ))}
                  </div>
                )}
                <div className="flex flex-wrap justify-end gap-2">
                  {node.state === "draft" && (
                    <>
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={busyId === node.id}
                        onClick={() => void review(node, "retired")}
                      >
                        <XIcon /> 退役
                      </Button>
                      <Button size="sm" disabled={busyId === node.id} onClick={() => void review(node, "accepted")}>
                        <CheckIcon /> 接受（进注入池）
                      </Button>
                    </>
                  )}
                  {node.state === "retired" && (
                    <>
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={busyId === node.id}
                        onClick={() => void remove(node)}
                      >
                        <XIcon /> 删除
                      </Button>
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={busyId === node.id}
                        onClick={() => void review(node, "draft")}
                      >
                        <HistoryIcon /> 恢复待评审
                      </Button>
                    </>
                  )}
                  {node.state === "accepted" && (
                    <Button
                      size="sm"
                      variant="outline"
                      disabled={busyId === node.id}
                      onClick={() => void review(node, "retired")}
                    >
                      <XIcon /> 退役（移出注入池）
                    </Button>
                  )}
                </div>
              </div>
            );
          })}
          {loaded && nodes.length > 0 && (
            <p className="flex flex-wrap items-center gap-1 text-[10px] text-muted-foreground">
              <HistoryIcon className="size-3" />
              只有 accepted 会注入新任务的规划上下文（预算 ≤12K 字符，top-5）。
              <Link
                href="/strategies/assembled"
                className="inline-flex items-center gap-0.5 text-primary hover:underline"
              >
                <LibraryBigIcon className="size-3" />
                看注入拼装全文
              </Link>
            </p>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
