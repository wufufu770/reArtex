"use client";

// 传承图页（P5 第三张图）：跨任务经验的列表 / 指纹 / 人工评审。
// 评审是人在环路的确定性闸：draft → accepted（进注入池）/ retired。无自动晋升。
//
// v0.10.1 三点（用户反馈）：
//   ① 长 summary（含蒸馏解析修复前的 JSON 大坨存量）默认折叠为 10 行，
//      「展开全文」看完整内容——折叠不只对 detail 生效；
//   ② draft 卡直接展示三门评估徽章（结构/复盘/证据）——「自动走流程，
//      显示在这里」，不等点了接受才弹含糊的错；
//   ③ 三门未过 ≠ 硬拒：带理由「越级接受（记录在案）」——门3 的实战 wins
//      只在接受注入后产生，新经验恒为 0，硬拒会让整条接受路死锁
//      （以人为本，与 SoD 改判同一哲学）。缺门 + 理由 + 操作人全量落审计。

import * as React from "react";

import Link from "next/link";

import { CheckIcon, ChevronDownIcon, ChevronUpIcon, HistoryIcon, LibraryBigIcon, XIcon } from "lucide-react";
import { toast } from "sonner";

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
import { Label } from "@/components/ui/label";
import { Spinner } from "@/components/ui/spinner";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
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

// 三门徽章的展示口径（key 与后端 missing 清单的门名前缀对齐）。
const GATE_CHIPS = [
  { key: "结构完整", label: "结构" },
  { key: "历史复盘", label: "复盘" },
  { key: "实战证据", label: "证据" },
] as const;

// summary 超过这个长度默认折叠（JSON 大坨存量 / 长蒸馏正文都适用）。
const SUMMARY_FOLD_LIMIT = 400;

type GateInfo = { pass: boolean; missing_gates: string[] };

export default function LegacyPage() {
  const [nodes, setNodes] = React.useState<LegacyNode[]>([]);
  const [state, setState] = React.useState("");
  const [loaded, setLoaded] = React.useState(false);
  const [busyId, setBusyId] = React.useState<number | null>(null);
  // 折叠状态（key = "s<id>" summary / "d<id>" detail）：默认收起、点开看全文。
  const [expanded, setExpanded] = React.useState<Set<string>>(() => new Set());
  // draft 卡的三门评估明细（自动逐条拉取）。
  const [gates, setGates] = React.useState<Record<number, GateInfo>>({});
  // 越级接受弹窗：三门未过时从 409 响应里带入缺门清单。
  const [override, setOverride] = React.useState<{ node: LegacyNode; missing: string[] } | null>(null);
  const [overrideReason, setOverrideReason] = React.useState("");
  const [accepting, setAccepting] = React.useState(false);

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

  // 三门自动评估：对每张 draft 卡拉一次明细（只读端点，不写任何东西）。
  React.useEffect(() => {
    const drafts = nodes.filter((n) => n.state === "draft");
    if (drafts.length === 0) return;
    let cancelled = false;
    void Promise.all(
      drafts.map((d) =>
        api
          .legacyGates(d.id)
          .then((g) => [d.id, { pass: g.pass, missing_gates: g.missing_gates ?? [] }] as const)
          .catch(() => null),
      ),
    ).then((rows) => {
      if (cancelled) return;
      const next: Record<number, GateInfo> = {};
      for (const r of rows) {
        if (r) next[r[0]] = r[1];
      }
      setGates(next);
    });
    return () => {
      cancelled = true;
    };
  }, [nodes]);

  function toggleFold(key: string) {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  }

  async function review(node: LegacyNode, next: "accepted" | "retired" | "draft") {
    setBusyId(node.id);
    try {
      await api.reviewLegacyNode(node.id, next);
      toast.success(REVIEW_TOAST[next] ?? "已更新");
      await load();
    } catch (e) {
      const err = e as Error & { missing_gates?: string[]; override_required?: boolean };
      if (next === "accepted" && err.override_required) {
        // 三门未过：进越级弹窗（明细展示 + 理由 + 记录在案的越级接受）。
        setOverride({ node, missing: err.missing_gates ?? [] });
        setOverrideReason("");
        return;
      }
      toast.error(`评审失败：${err.message}`);
    } finally {
      setBusyId(null);
    }
  }

  async function acceptOverride() {
    if (!override) return;
    setAccepting(true);
    try {
      await api.reviewLegacyNode(override.node.id, "accepted", {
        override: true,
        reason: overrideReason.trim(),
      });
      toast.success("已越级接受（缺门清单与理由已落审计，全程可溯）");
      setOverride(null);
      await load();
    } catch (e) {
      toast.error(`越级接受失败：${(e as Error).message}`);
    } finally {
      setAccepting(false);
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
          <CardDescription>
            draft 只有不进注入池；accepted
            才会在新同族任务规划时被携带。三门评估自动给出明细：未过可带理由越级接受（记录在案）。
          </CardDescription>
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
            const summaryLong = summary.length > SUMMARY_FOLD_LIMIT;
            const summaryOpen = expanded.has(`s${node.id}`);
            const detailOpen = expanded.has(`d${node.id}`);
            const gateInfo = gates[node.id];
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
                {/* 三门评估徽章（draft 卡自动展示）：未过的门给琥珀色，hover 看原因 */}
                {node.state === "draft" && gateInfo && (
                  <div className="flex flex-wrap items-center gap-1">
                    <span className="text-[10px] text-muted-foreground">三门：</span>
                    {GATE_CHIPS.map((g) => {
                      const missingItem = gateInfo.missing_gates.find((m) => m.startsWith(g.key));
                      return (
                        <Badge
                          key={g.key}
                          variant="outline"
                          title={missingItem ?? "通过"}
                          className={`px-1.5 py-0 text-[10px] ${
                            missingItem
                              ? "border-amber-500/40 text-amber-600"
                              : "border-emerald-500/40 text-emerald-600"
                          }`}
                        >
                          {missingItem ? "✗" : "✓"} {g.label}
                        </Badge>
                      );
                    })}
                    {!gateInfo.pass && (
                      <span className="text-[10px] text-muted-foreground">
                        未过可带理由越级接受（记录在案；门3 的实战 wins 只在接受注入后才产生，新经验恒为 0）
                      </span>
                    )}
                  </div>
                )}
                {summary !== "" && summary !== "[]" && (
                  <div className="grid gap-1">
                    <p
                      className={`whitespace-pre-wrap text-muted-foreground text-xs ${
                        summaryLong && !summaryOpen ? "line-clamp-10" : ""
                      }`}
                    >
                      {summary}
                    </p>
                    {summaryLong && (
                      <Button
                        size="sm"
                        variant="ghost"
                        className="h-6 w-fit px-2 text-[11px]"
                        onClick={() => toggleFold(`s${node.id}`)}
                      >
                        {summaryOpen ? <ChevronUpIcon /> : <ChevronDownIcon />}
                        {summaryOpen ? "收起摘要" : `展开摘要（${summary.length} 字）`}
                      </Button>
                    )}
                  </div>
                )}
                {/* 蒸馏正文（detail）：默认收起 6 行，展开看全文（v0.9.17 用户拍板） */}
                {detail !== "" && detail !== summary && (
                  <div className="grid gap-1">
                    <p className={`whitespace-pre-wrap text-xs ${detailOpen ? "" : "line-clamp-6"}`}>{detail}</p>
                    <Button
                      size="sm"
                      variant="ghost"
                      className="h-6 w-fit px-2 text-[11px]"
                      onClick={() => toggleFold(`d${node.id}`)}
                    >
                      {detailOpen ? <ChevronUpIcon /> : <ChevronDownIcon />}
                      {detailOpen ? "收起正文" : "展开正文"}
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

      {/* 越级接受弹窗：三门明细 + 理由（以人为本，缺门与理由全量落审计） */}
      <Dialog
        open={override !== null}
        onOpenChange={(open) => {
          if (!open) setOverride(null);
        }}
      >
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>三门未过 · 越级接受</DialogTitle>
            <DialogDescription>
              经验 #{override?.node.id} 未过以下三门。人工接受即授权进注入池（缺门清单与理由将记录在案，
              实战表现由经验权重卡后续跟踪；门3 的 wins 只在接受注入后才产生，新经验恒为 0）。
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-3 py-1">
            <div className="grid gap-1.5">
              {(override?.missing ?? []).map((m) => (
                <p
                  key={m}
                  className="rounded-md border border-amber-500/30 bg-amber-500/10 px-2.5 py-1.5 text-xs text-amber-600 dark:text-amber-400"
                >
                  ✗ {m}
                </p>
              ))}
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="legacy-override-reason">越级理由（审计留痕，必填）</Label>
              <Textarea
                id="legacy-override-reason"
                className="min-h-20"
                value={overrideReason}
                onChange={(e) => setOverrideReason(e.target.value)}
                placeholder="例：内容已人工核阅，与当次任务事实相符；先入池观察实战表现"
              />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setOverride(null)} disabled={accepting}>
              取消
            </Button>
            <Button onClick={() => void acceptOverride()} disabled={accepting || !overrideReason.trim()}>
              {accepting && <Spinner data-icon="inline-start" />}
              越级接受（记录在案）
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
