"use client";

// 业务建模 tab（BIZ 合并施工 W-7/W-8）：本次建模的业务图摘要（design/behavior
// 节点数、未决差异）+ 差异清单 + 偏锋判据进度（任务 edge_mode 时显示）。
// 图的可交互全量视图在 /function/business-graph，这里给任务维度的增量视角。

import * as React from "react";

import { StatusBadge } from "@/components/status-badge";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { api } from "@/lib/api";
import type { BizLayerDiff, EdgeCell } from "@/lib/types";

const CELL_STATE_LABEL: Record<EdgeCell["state"], string> = {
  open: "未开",
  dispatched: "已下发",
  confirmed: "已确证",
  disproven: "已证伪",
  folded: "封格",
};

const _CELL_STATE_TONE: Record<EdgeCell["state"], "blue" | "amber" | "green" | "red" | "slate"> = {
  open: "slate",
  dispatched: "amber",
  confirmed: "green",
  disproven: "red",
  folded: "slate",
};

export function BizTab({ taskId }: { taskId: string }) {
  const [summary, setSummary] = React.useState<{
    design_nodes: number;
    behavior_nodes: number;
    open_diffs: number;
  } | null>(null);
  const [nodes, setNodes] = React.useState<
    { id: number; name: string; node_type: string; layer: string; confidence: string }[]
  >([]);
  const [edgesCount, setEdgesCount] = React.useState(0);
  const [diffs, setDiffs] = React.useState<BizLayerDiff[]>([]);
  const [edge, setEdge] = React.useState<(EdgeCell[] & { _meta?: unknown }) | null>(null);
  const [edgeMeta, setEdgeMeta] = React.useState<{
    edge_mode: boolean;
    methodology_id: string;
    criterion_progress: number;
    stop_stats: Record<string, number>;
  } | null>(null);
  const [loading, setLoading] = React.useState(false);

  const load = React.useCallback(() => {
    setLoading(true);
    Promise.all([
      api.taskBizGraph(taskId).catch(() => null),
      api.bizDiffs({ task_id: Number(taskId), limit: 100 }).catch(() => null),
      api.taskEdgeCoverage(taskId).catch(() => null),
    ])
      .then(([g, ds, ec]) => {
        if (g) {
          setSummary(g.summary);
          setNodes(
            (g.nodes ?? []).slice(0, 200).map((n) => ({
              id: n.id,
              name: n.name,
              node_type: n.node_type,
              layer: n.layer,
              confidence: n.confidence,
            })),
          );
          setEdgesCount((g.edges ?? []).length);
        }
        if (ds) setDiffs(ds.diffs ?? []);
        if (ec) {
          setEdge(ec.cells ?? []);
          setEdgeMeta({
            edge_mode: ec.edge_mode,
            methodology_id: ec.methodology_id,
            criterion_progress: ec.criterion_progress,
            stop_stats: ec.stop_stats ?? {},
          });
        }
      })
      .finally(() => setLoading(false))
      .catch(() => setLoading(false)); // 各请求已自带 catch 兜底；这里的 catch 只兜 finally 链本身
  }, [taskId]);

  React.useEffect(() => {
    load();
  }, [load]);

  return (
    <div className="space-y-4 p-4">
      <div className="flex items-center gap-2">
        <Button variant="outline" size="sm" className="h-8 text-xs" onClick={load} disabled={loading}>
          {loading ? "加载中…" : "刷新"}
        </Button>
        <span className="text-muted-foreground text-xs">
          业务图全量视图见侧边栏「业务图」页；派生与裁决动作走「业务建模」agent 对话。
        </span>
      </div>

      {/* 摘要 chips */}
      <div className="grid gap-2 sm:grid-cols-4">
        <div className="rounded-lg border p-3">
          <div className="text-muted-foreground text-xs">design 层节点（声称）</div>
          <div className="font-semibold text-2xl">{summary?.design_nodes ?? "—"}</div>
        </div>
        <div className="rounded-lg border p-3">
          <div className="text-muted-foreground text-xs">behavior 层节点（实际）</div>
          <div className="font-semibold text-2xl">{summary?.behavior_nodes ?? "—"}</div>
        </div>
        <div className="rounded-lg border p-3">
          <div className="text-muted-foreground text-xs">未决差异</div>
          <div className="font-semibold text-2xl">{summary?.open_diffs ?? "—"}</div>
        </div>
        <div className="rounded-lg border p-3">
          <div className="text-muted-foreground text-xs">关系总数</div>
          <div className="font-semibold text-2xl">{edgesCount || "—"}</div>
        </div>
      </div>

      {/* 偏锋判据进度（edge_mode 任务才显示） */}
      {edgeMeta?.edge_mode && (
        <div className="rounded-lg border p-3">
          <div className="mb-2 flex items-center gap-2 font-medium text-sm">
            偏锋判据进度
            <Badge variant="outline" className="text-[10px]">
              {edgeMeta.methodology_id || "—"}
            </Badge>
            <Badge variant="outline" className="text-[10px]">
              判据完成度 {Math.round((edgeMeta.criterion_progress || 0) * 100)}%
            </Badge>
          </div>
          {edge && edge.length > 0 ? (
            <div className="flex flex-wrap gap-1">
              {edge.map((c) => (
                <Badge key={c.id} variant="outline" className="text-[10px]">
                  {c.cell_id} · {CELL_STATE_LABEL[c.state]} · {c.attempts}次
                </Badge>
              ))}
            </div>
          ) : (
            <div className="text-muted-foreground text-xs">尚无格点（edge_run / edge_activate 初始化坐标系）。</div>
          )}
          {edgeMeta.stop_stats && Object.keys(edgeMeta.stop_stats).length > 0 && (
            <div className="mt-2 text-muted-foreground text-xs">
              历史运行终止原因：
              {Object.entries(edgeMeta.stop_stats)
                .map(([k, v]) => `${k}×${v}`)
                .join("、")}
            </div>
          )}
        </div>
      )}

      {/* 差异清单 */}
      <div className="rounded-lg border">
        <div className="border-b px-3 py-2 font-medium text-sm">本任务差异（biz_layer_diffs）</div>
        <div className="divide-y">
          {diffs.length === 0 && (
            <div className="px-3 py-6 text-center text-muted-foreground text-xs">本任务暂无差异记录。</div>
          )}
          {diffs.map((d) => (
            <div key={d.id} className="flex flex-wrap items-center gap-2 px-3 py-2 text-xs">
              <StatusBadge domain="finding" value={d.severity} />
              <span className="font-medium">{d.diff_type}</span>
              <Badge variant="outline" className="text-[10px]">
                {d.wstg_ref || "—"}
              </Badge>
              <span className="text-muted-foreground">{d.description}</span>
              <Badge variant="outline" className="ml-auto text-[10px]">
                {d.status}
              </Badge>
            </div>
          ))}
        </div>
      </div>

      {/* 节点简表 */}
      {nodes.length > 0 && (
        <div className="rounded-lg border">
          <div className="border-b px-3 py-2 font-medium text-sm">业务节点（前 {nodes.length} 个）</div>
          <div className="max-h-72 overflow-y-auto">
            <table className="w-full text-xs">
              <tbody className="divide-y">
                {nodes.map((n) => (
                  <tr key={n.id} className="px-3">
                    <td className="px-3 py-1.5 font-medium">
                      #{n.id} {n.name}
                    </td>
                    <td className="px-3 py-1.5 text-muted-foreground">{n.node_type}</td>
                    <td className="px-3 py-1.5 text-muted-foreground">{n.layer}</td>
                    <td className="px-3 py-1.5 text-right text-muted-foreground">{n.confidence}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </div>
  );
}
