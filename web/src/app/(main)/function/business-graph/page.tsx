"use client";

// 业务建模图（第三张图）主视图（BIZ 合并施工 W-4）。
//
// 三种视图按既定口径：语义视图（全部节点，力导向）、权限视图（角色×授权×动作×
// 资源）、状态机视图（状态与 transitions）。双层图谱按「切换 + diff 高亮」呈现，
// design/behavior 不叠加显示以避免视觉噪声；diff 用 severity 色高亮在当前层上。
// 图形手法沿用 coverage-graph-tab（lucide 图标白描边 data URI 作 iconSrc）。

import * as React from "react";

import Link from "next/link";

import type { Graph as G6Graph } from "@antv/g6";

import { EdgeOverviewPanel } from "@/components/edge-overview-panel";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api } from "@/lib/api";
import { statusMeta } from "@/lib/status";
import type { BizGraphNode, BizLayerDiff, Company } from "@/lib/types";
import { usePreferencesStore } from "@/stores/preferences/preferences-provider";

// ---------- 图元定义（节点类型 → lucide path 白描边 data URI，照 coverage-graph-tab 手法）----------

type BizNodeType = BizGraphNode["node_type"];

const svgUri = (inner: string): string =>
  `data:image/svg+xml,${encodeURIComponent(
    `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="#fff" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">${inner}</svg>`,
  )}`;

const TYPE_ICON: Record<BizNodeType, string> = {
  biz_entity: svgUri(
    '<rect x="3" y="7" width="18" height="13" rx="2"/><path d="M8 7V5a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/>',
  ),
  biz_action: svgUri('<polygon points="6 3 20 12 6 21 6 3"/>'),
  biz_state: svgUri('<circle cx="12" cy="12" r="8"/><circle cx="12" cy="12" r="3"/>'),
  biz_role: svgUri('<circle cx="12" cy="8" r="4"/><path d="M4 21v-2a6 6 0 0 1 12 0v2"/>'),
  biz_trust: svgUri('<path d="M12 3l8 4v5c0 5-3.5 8-8 9-4.5-1-8-4-8-9V7z"/>'),
  biz_dataflow: svgUri('<path d="M4 17c4-8 12 0 16-8"/><path d="M17 5h3v3"/><path d="M4 7h4"/>'),
  biz_external: svgUri(
    '<circle cx="12" cy="12" r="9"/><path d="M3 12h18"/><path d="M12 3a14 14 0 0 1 0 18a14 14 0 0 1 0-18"/>',
  ),
};

const TYPE_META: Record<BizNodeType, { label: string; hex: string; size: number }> = {
  biz_entity: { label: "实体", hex: "#6366f1", size: 36 },
  biz_action: { label: "动作", hex: "#f59e0b", size: 34 },
  biz_state: { label: "状态", hex: "#10b981", size: 32 },
  biz_role: { label: "角色", hex: "#8b5cf6", size: 34 },
  biz_trust: { label: "信任边界", hex: "#ef4444", size: 38 },
  biz_dataflow: { label: "数据流", hex: "#06b6d4", size: 32 },
  biz_external: { label: "外部系统", hex: "#64748b", size: 36 },
};

const REL_COLOR: Record<string, string> = {
  owns: "#8b5cf6",
  invokes: "#f59e0b",
  depends_on: "#64748b",
  trusts: "#ef4444",
  flows_to: "#06b6d4",
  validates: "#10b981",
  requires_state: "#6366f1",
  transitions: "#10b981",
  grants: "#8b5cf6",
};

// severityHexFor 是 severity → 描边色的模块级映射（statusMeta 的 finding 域；
// 放模块层避免每渲染新引用污染 hook 依赖）。
function severityHexFor(sev: string): string {
  const tone = statusMeta("finding", sev).tone;
  if (tone === "rose") return "#e11d48";
  if (sev === "high") return "#dc2626";
  if (sev === "medium") return "#d97706";
  return "#64748b";
}

type LayerFilter = "all" | "design" | "behavior";
type ViewMode = "semantic" | "permission" | "state";

// 权限视图：角色 + grants/owns/invokes/validates 相关子图。
// 状态机视图：状态节点 + transitions/requires_state 边及两端动作。
const VIEW_KEEP_EDGE: Record<ViewMode, (rel: string) => boolean> = {
  semantic: () => true,
  permission: (rel) => ["grants", "owns", "invokes", "validates"].includes(rel),
  state: (rel) => ["transitions", "requires_state"].includes(rel),
};

function _nd(d: unknown): BizGraphNode {
  return (d as { bizNode?: BizGraphNode }).bizNode ?? (d as unknown as BizGraphNode);
}

export function BusinessGraphPage() {
  // 页面双模式（0.11.8 重构）：graph=业务图（默认）；edge=偏锋探索总览。
  // 偏锋总览不再是独立静态路由（独立路由是该功能点唯一与其它可达页面的结构差异，
  // 用户 Safari 上反复无法载入）——sidebar「偏锋探索」= 本页 + ?mode=edge。
  // 初始 mode 从 URL query 读（静态导出下用 window.location，绕开
  // useSearchParams 的 CSR bailout 限制）。
  const [mode, setMode] = React.useState<"graph" | "edge">("graph");
  React.useEffect(() => {
    if (typeof window !== "undefined" && new URLSearchParams(window.location.search).get("mode") === "edge") {
      setMode("edge");
    }
  }, []);

  const containerRef = React.useRef<HTMLDivElement | null>(null);
  const graphRef = React.useRef<G6Graph | null>(null);
  const renderMapRef = React.useRef<Map<string, BizGraphNode>>(new Map());

  const [companies, setCompanies] = React.useState<Company[]>([]);
  const [companyId, setCompanyId] = React.useState<number>(0);
  const [layer, setLayer] = React.useState<LayerFilter>("all");
  const [view, setView] = React.useState<ViewMode>("semantic");
  const [showDiffOnly, setShowDiffOnly] = React.useState(false);
  const [nodes, setNodes] = React.useState<BizGraphNode[]>([]);
  const [edges, setEdges] = React.useState<
    { id: number; src_id: number; dst_id: number; rel: string; layer: string; confidence: string }[]
  >([]);
  const [diffs, setDiffs] = React.useState<BizLayerDiff[]>([]);
  const [selected, setSelected] = React.useState<BizGraphNode | null>(null);
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState("");
  // 画布配色跟随页面主题（resolvedThemeMode 由偏好 store 同步 DOM data-theme）。
  const theme = usePreferencesStore((s) => s.resolvedThemeMode);

  React.useEffect(() => {
    api
      .companies()
      .then((cs) => {
        setCompanies(cs);
        if (cs.length > 0) setCompanyId((prev) => prev || cs[0].id);
      })
      .catch(() => setError("公司清单加载失败"));
  }, []);

  const load = React.useCallback(() => {
    if (!companyId) return;
    setLoading(true);
    setError("");
    Promise.all([
      api.bizGraph(companyId, undefined, layer === "all" ? undefined : layer),
      api.bizDiffs({ company_id: companyId, limit: 200 }),
    ])
      .then(([g, ds]) => {
        setNodes(g.nodes ?? []);
        setEdges(
          (g.edges ?? []).map((e) => ({
            id: e.id,
            src_id: e.src_id,
            dst_id: e.dst_id,
            rel: e.rel,
            layer: e.layer,
            confidence: e.confidence,
          })),
        );
        setDiffs(ds.diffs ?? []);
      })
      .catch((err) => setError(String(err)))
      .finally(() => setLoading(false));
  }, [companyId, layer]);

  React.useEffect(() => {
    load();
  }, [load]);

  // diff 高亮集合：命中任一 diff 的 node_ids 的节点 → severity 描边。
  const diffNodeSev = React.useMemo(() => {
    const m = new Map<number, BizLayerDiff["severity"]>();
    const sevRank: Record<string, number> = { low: 1, medium: 2, high: 3, critical: 4 };
    for (const d of diffs) {
      if (d.status === "false_positive") continue;
      for (const ref of [d.design_ref, d.behavior_ref]) {
        const ids = (ref as { node_ids?: unknown })?.node_ids;
        if (!Array.isArray(ids)) continue;
        for (const v of ids) {
          const id = Number(v);
          if (!id) continue;
          const cur = m.get(id);
          if (!cur || sevRank[d.severity] > sevRank[cur]) m.set(id, d.severity);
        }
      }
    }
    return m;
  }, [diffs]);

  // 视图过滤。
  const visibleNodes = React.useMemo(() => {
    let out = nodes;
    if (view === "permission")
      out = out.filter(
        (n) => n.node_type === "biz_role" || n.node_type === "biz_action" || n.node_type === "biz_entity",
      );
    if (view === "state") out = out.filter((n) => n.node_type === "biz_state" || n.node_type === "biz_action");
    if (showDiffOnly && diffNodeSev.size > 0) out = out.filter((n) => diffNodeSev.has(n.id));
    return out;
  }, [nodes, view, showDiffOnly, diffNodeSev]);

  const visibleEdges = React.useMemo(() => {
    const ids = new Set(visibleNodes.map((n) => n.id));
    return edges.filter((e) => ids.has(e.src_id) && ids.has(e.dst_id) && VIEW_KEEP_EDGE[view](e.rel));
  }, [visibleNodes, edges, view]);

  const severityHex = severityHexFor;

  // 数据应用（图已建时增量更新）。
  const applyData = React.useCallback(() => {
    const graph = graphRef.current;
    if (!graph) return;
    const nodeData = visibleNodes.map((n) => {
      const meta = TYPE_META[n.node_type];
      const sev = diffNodeSev.get(n.id);
      renderMapRef.current.set(`b${n.id}`, n);
      return {
        id: `b${n.id}`,
        bizNode: n,
        size: meta.size,
        fill: meta.hex,
        stroke: sev ? severityHex(sev) : "#ffffff",
        lineWidth: sev ? 4 : 1.5,
        iconSrc: TYPE_ICON[n.node_type],
        labelText: n.name,
        layer: n.layer,
      };
    });
    const edgeData = visibleEdges.map((e) => ({
      source: `b${e.src_id}`,
      target: `b${e.dst_id}`,
      stroke: REL_COLOR[e.rel] ?? "#94a3b8",
      lineWidth: e.confidence === "observed" ? 2.2 : 1.2,
      lineDash: e.confidence === "observed" ? [] : [4, 3],
      labelText: e.rel,
      labelFontSize: 9,
      labelFill: "#64748b",
      labelBackground: true,
      labelBackgroundFill: "rgba(255,255,255,0.7)",
      labelPadding: [0, 2],
    }));
    graph.setData({ nodes: nodeData, edges: edgeData });
    void graph.render().catch((err: unknown) => {
      if (graphRef.current && !graphRef.current.destroyed) console.error("[business-graph] render:", err);
    });
  }, [visibleNodes, visibleEdges, diffNodeSev]);

  React.useEffect(() => {
    if (graphRef.current) applyData();
  }, [applyData]);

  // 建图（主题切换时销毁重建；数据由上方 applyData effect 单独灌入——
  // 数据变化不重建整图）。画布配色跟随页面主题：G6 画布背景是硬编码 hex，
  // 不跟随 CSS 变量，所以按 resolvedThemeMode 手动给两套（暗色下不再刺眼）。
  const dark = theme === "dark";
  const canvas = React.useMemo(
    () =>
      dark
        ? { bg: "#111827", labelFill: "#94a3b8", labelBg: "rgba(17,24,39,0.78)", edge: "#475569", edgeLabel: "#94a3b8" }
        : {
            bg: "#f0f2f7",
            labelFill: "#475569",
            labelBg: "rgba(255,255,255,0.75)",
            edge: "#94a3b8",
            edgeLabel: "#64748b",
          },
    [dark],
  );
  // applyData 走 ref：建图 effect 只依赖 canvas（主题），数据灌入始终用最新闭包
  // ——数据变化不重建整图（重建由上方 applyData effect 独立负责）。
  const applyDataRef = React.useRef(applyData);
  applyDataRef.current = applyData;
  React.useEffect(() => {
    let destroyed = false;
    let graph: G6Graph | null = null;
    void (async () => {
      const { Graph } = await import("@antv/g6");
      if (destroyed || !containerRef.current) return;
      graph = new Graph({
        container: containerRef.current,
        autoResize: true,
        autoFit: "view",
        background: canvas.bg,
        node: {
          style: {
            size: (d: unknown) => (d as { size?: number }).size ?? 34,
            fill: (d: unknown) => (d as { fill?: string }).fill ?? "#6366f1",
            stroke: (d: unknown) => (d as { stroke?: string }).stroke ?? "#fff",
            lineWidth: (d: unknown) => (d as { lineWidth?: number }).lineWidth ?? 1.5,
            iconSrc: (d: unknown) => (d as { iconSrc?: string }).iconSrc,
            iconWidth: 16,
            iconHeight: 16,
            labelText: (d: unknown) => (d as { labelText?: string }).labelText ?? "",
            labelFontSize: 10,
            labelPlacement: "bottom",
            labelFill: canvas.labelFill,
            labelBackground: true,
            labelBackgroundFill: canvas.labelBg,
            labelBackgroundRadius: 3,
            labelPadding: [1, 3],
          },
        },
        edge: {
          style: {
            stroke: (d: unknown) => (d as { stroke?: string }).stroke ?? canvas.edge,
            lineWidth: (d: unknown) => (d as { lineWidth?: number }).lineWidth ?? 1,
            lineDash: (d: unknown) => (d as { lineDash?: number[] }).lineDash ?? [],
            labelText: (d: unknown) => (d as { labelText?: string }).labelText ?? "",
            labelFontSize: (d: unknown) => (d as { labelFontSize?: number }).labelFontSize ?? 9,
            labelFill: (d: unknown) => (d as { labelFill?: string }).labelFill ?? canvas.edgeLabel,
            labelBackground: (d: unknown) => (d as { labelBackground?: boolean }).labelBackground ?? false,
            labelBackgroundFill: (d: unknown) =>
              (d as { labelBackgroundFill?: string }).labelBackgroundFill ?? canvas.labelBg,
            labelPadding: (d: unknown) => (d as { labelPadding?: number[] }).labelPadding,
          },
        },
        layout: {
          type: "d3-force",
          collide: { radius: 40 },
          link: { distance: 90 },
          manyBody: { strength: -180 },
        },
        behaviors: ["drag-element-force", "drag-canvas", "zoom-canvas"],
      });
      graph.on("node:click", (evt: unknown) => {
        const id = (evt as { target?: { id?: string } }).target?.id;
        if (!id) return;
        const n = renderMapRef.current.get(id);
        if (n) setSelected(n);
      });
      graphRef.current = graph;
      applyDataRef.current();
    })();
    return () => {
      destroyed = true;
      try {
        graph?.stopLayout();
      } catch {
        /* 图可能尚未建成 */
      }
      graph?.destroy();
      graphRef.current = null;
    };
  }, [canvas]);

  const selectedDiffs = React.useMemo(
    () =>
      diffs.filter((d) => {
        const ids = [d.design_ref, d.behavior_ref].flatMap((ref) =>
          Array.isArray((ref as { node_ids?: unknown })?.node_ids)
            ? ((ref as { node_ids: unknown[] }).node_ids as unknown[]).map(Number)
            : [],
        );
        return selected && ids.includes(selected.id);
      }),
    [diffs, selected],
  );

  return (
    <div className="flex h-full flex-col gap-3 p-4">
      {/* 页面模式切换：业务图 / 偏锋探索（偏锋总览并入本页，见组件头注释） */}
      <div className="flex flex-wrap items-center gap-2">
        <Tabs value={mode} onValueChange={(v) => setMode(v as "graph" | "edge")}>
          <TabsList className="h-8">
            <TabsTrigger value="graph" className="text-xs">
              业务图
            </TabsTrigger>
            <TabsTrigger value="edge" className="text-xs">
              偏锋探索
            </TabsTrigger>
          </TabsList>
        </Tabs>
        <Link href="/function/business-graph/diffs" className="text-xs underline-offset-2 hover:underline">
          差异面板 →
        </Link>
      </div>
      {mode === "edge" && <EdgeOverviewPanel />}
      {mode === "graph" && (
        <>
          <div className="flex flex-wrap items-center gap-2">
            <select
              className="h-8 rounded-md border bg-background px-2 text-sm"
              value={companyId}
              onChange={(e) => setCompanyId(Number(e.target.value))}
            >
              {companies.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name}
                </option>
              ))}
            </select>
            <Tabs value={layer} onValueChange={(v) => setLayer(v as LayerFilter)}>
              <TabsList className="h-8">
                <TabsTrigger value="all" className="text-xs">
                  双层
                </TabsTrigger>
                <TabsTrigger value="design" className="text-xs">
                  design（声称）
                </TabsTrigger>
                <TabsTrigger value="behavior" className="text-xs">
                  behavior（实际）
                </TabsTrigger>
              </TabsList>
            </Tabs>
            <Tabs value={view} onValueChange={(v) => setView(v as ViewMode)}>
              <TabsList className="h-8">
                <TabsTrigger value="semantic" className="text-xs">
                  语义视图
                </TabsTrigger>
                <TabsTrigger value="permission" className="text-xs">
                  权限视图
                </TabsTrigger>
                <TabsTrigger value="state" className="text-xs">
                  状态机视图
                </TabsTrigger>
              </TabsList>
            </Tabs>
            <Button
              variant={showDiffOnly ? "default" : "outline"}
              size="sm"
              className="h-8 text-xs"
              onClick={() => setShowDiffOnly((v) => !v)}
            >
              仅看差异节点
            </Button>
            <Button variant="outline" size="sm" className="h-8 text-xs" onClick={load} disabled={loading}>
              {loading ? "加载中…" : "刷新"}
            </Button>
            <div className="ml-auto flex items-center gap-2 text-muted-foreground text-xs">
              <span>
                {visibleNodes.length} 节点 / {visibleEdges.length} 关系
              </span>
              <Badge variant="outline" className="text-[10px]">
                描边=差异 severity（红橙黄）
              </Badge>
            </div>
          </div>
          {error && (
            <div className="rounded-md border border-red-500/30 bg-red-500/10 px-3 py-2 text-red-600 text-xs">
              {error}
            </div>
          )}
          <div className="relative min-h-0 flex-1 overflow-hidden rounded-lg border">
            <div ref={containerRef} className="h-full w-full" />
            {visibleNodes.length === 0 && !loading && (
              <div className="pointer-events-none absolute inset-0 flex items-center justify-center text-muted-foreground text-sm">
                暂无业务图数据
              </div>
            )}
          </div>
          <Sheet
            open={selected !== null}
            onOpenChange={(open) => {
              if (!open) setSelected(null);
            }}
          >
            <SheetContent side="right" className="overflow-y-auto data-[side=right]:sm:max-w-md">
              {selected && (
                <>
                  <SheetHeader>
                    <SheetTitle className="flex items-center gap-2">
                      <span
                        className="inline-block size-3 rounded-full"
                        style={{ background: TYPE_META[selected.node_type].hex }}
                      />
                      {selected.name}
                    </SheetTitle>
                    <SheetDescription>
                      {TYPE_META[selected.node_type].label} ·{" "}
                      {selected.layer === "design" ? "design（声称）" : "behavior（实际）"} ·{" "}
                      {selected.confidence === "observed" ? "observed（直接看到）" : "inferred（推断）"}
                    </SheetDescription>
                  </SheetHeader>
                  <div className="mt-2 space-y-3 text-sm">
                    {selected.raw_name && selected.raw_name !== selected.name && (
                      <div className="text-muted-foreground text-xs">原始名：{selected.raw_name}</div>
                    )}
                    {selected.description && <p className="text-muted-foreground">{selected.description}</p>}
                    <div className="flex flex-wrap gap-1">
                      {selected.asset_ids.map((aid) => (
                        <Badge key={aid} variant="outline" className="text-[10px]">
                          资产 #{aid}
                        </Badge>
                      ))}
                    </div>
                    {selected.props && Object.keys(selected.props as Record<string, unknown>).length > 0 && (
                      <pre className="max-h-40 overflow-auto rounded-md bg-muted p-2 text-[11px]">
                        {JSON.stringify(selected.props, null, 2)}
                      </pre>
                    )}
                    <div>
                      <div className="mb-1 font-medium text-xs">相关差异</div>
                      {selectedDiffs.length === 0 && <div className="text-muted-foreground text-xs">无</div>}
                      {selectedDiffs.map((d) => (
                        <div key={d.id} className="mb-1 rounded-md border p-2 text-xs">
                          <span className="font-medium">{d.diff_type}</span> · {d.wstg_ref || "—"} /{" "}
                          {d.stride_ref || "—"} · <span className="text-muted-foreground">{d.status}</span>
                          <div className="text-muted-foreground">{d.description}</div>
                        </div>
                      ))}
                    </div>
                  </div>
                </>
              )}
            </SheetContent>
          </Sheet>
        </>
      )}
    </div>
  );
}

export default BusinessGraphPage;
