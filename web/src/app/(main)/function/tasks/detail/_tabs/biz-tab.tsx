"use client";

// 业务建模 tab（BIZ 合并施工 W-7/W-8）：本次建模的业务图摘要（design/behavior
// 节点数、未决差异）+ 差异清单 + 偏锋判据进度（任务 edge_mode 时显示）。
// 图的可交互全量视图在 /function/business-graph，这里给任务维度的增量视角。

import * as React from "react";

import { toast } from "sonner";

import { StatusBadge } from "@/components/status-badge";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { api } from "@/lib/api";
import type { BizLayerDiff, CoverageGap, EdgeCell, IntentConflict } from "@/lib/types";

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
  const [hostTargets, _setHostTargets] = React.useState<string[]>([]);
  const [hostInput, setHostInput] = React.useState("");
  const [hostDirty, setHostDirty] = React.useState(false);
  const [loading, setLoading] = React.useState(false);
  // 意图冲突仲裁（0.14.2）：冲突对清单 + 人工重开（理由必填，写审计链）。
  const [conflicts, setConflicts] = React.useState<IntentConflict[]>([]);
  const [reopenID, setReopenID] = React.useState<number | null>(null);
  const [reopenReason, setReopenReason] = React.useState("");
  // 覆盖缺口（0.14.3）：链接到任务但无 open 意图锚的资产（对账在调度器重扫轮）。
  const [gaps, setGaps] = React.useState<CoverageGap[]>([]);
  const [closeGapID, setCloseGapID] = React.useState<number | null>(null);
  const [closeGapReason, setCloseGapReason] = React.useState("");

  const load = React.useCallback(() => {
    setLoading(true);
    Promise.all([
      api.taskBizGraph(taskId).catch(() => null),
      api.bizDiffs({ task_id: Number(taskId), limit: 100 }).catch(() => null),
      api.taskEdgeCoverage(taskId).catch(() => null),
      api.taskHostTargets(taskId).catch(() => ({ targets: [] })),
      api.intentConflicts(taskId).catch(() => null),
      api.coverageGaps(taskId).catch(() => null),
    ])
      .then(([g, ds, ec, _ht, cf, gp]) => {
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
        if (cf) setConflicts(cf.conflicts ?? []);
        if (gp) setGaps(gp.gaps ?? []);
      })
      .finally(() => setLoading(false))
      .catch(() => setLoading(false)); // 各请求已自带 catch 兜底；这里的 catch 只兜 finally 链本身
  }, [taskId]);

  const reopenConflict = async (id: number) => {
    if (!reopenReason.trim()) {
      toast.error("重开理由必填（写审计链）");
      return;
    }
    try {
      await api.intentConflictReopen(id, reopenReason.trim());
      toast.success(`冲突 #${id} 已重开（被压制的意图已恢复待命）`);
      setReopenID(null);
      setReopenReason("");
      load();
    } catch (e) {
      toast.error(`重开失败：${(e as Error).message}`);
    }
  };

  const closeGap = async (id: number) => {
    if (!closeGapReason.trim()) {
      toast.error("闭合理由必填（写审计链）");
      return;
    }
    try {
      await api.coverageGapClose(taskId, id, closeGapReason.trim());
      toast.success(`缺口 #${id} 已闭合（审计链已记）`);
      setCloseGapID(null);
      setCloseGapReason("");
      load();
    } catch (e) {
      toast.error(`闭合失败：${(e as Error).message}`);
    }
  };

  React.useEffect(() => {
    load();
  }, [load]);

  return (
    <div className="space-y-4 p-4">
      <div className="flex items-center gap-2">
        <Button variant="outline" size="sm" className="h-8 text-xs" onClick={load} disabled={loading}>
          {loading ? "加载中…" : "刷新"}
        </Button>
        <span className="text-muted-foreground text-xs" />
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

      {/* 覆盖缺口（0.14.3）：链接到任务但无 open 意图锚的资产；targeted=已有意图认领 */}
      {gaps.length > 0 && (
        <div className="rounded-lg border p-3">
          <div className="mb-2 flex items-center gap-2 font-medium text-sm">
            覆盖缺口
            <Badge variant="outline" className="text-[10px]">
              {gaps.filter((g) => g.state !== "closed").length} 未闭合
            </Badge>
          </div>
          <div className="space-y-2">
            {gaps.map((g) => (
              <div key={g.id} className="flex flex-wrap items-center gap-2 rounded-md border px-2 py-1.5">
                <span className="font-mono text-xs">#{g.id}</span>
                <Badge
                  variant={g.state === "open" ? "destructive" : g.state === "targeted" ? "secondary" : "outline"}
                  className="text-[10px]"
                >
                  {g.state}
                </Badge>
                <span className="text-muted-foreground text-xs">资产 #{g.asset_id}</span>
                {g.state !== "closed" && closeGapID !== g.id && (
                  <Button
                    variant="outline"
                    size="sm"
                    className="ml-auto h-6 text-[10px]"
                    onClick={() => {
                      setCloseGapID(g.id);
                      setCloseGapReason("");
                    }}
                  >
                    闭合
                  </Button>
                )}
                {closeGapID === g.id && (
                  <div className="flex w-full flex-wrap items-center gap-1.5">
                    <input
                      className="h-7 w-72 rounded-md border bg-background px-2 text-xs"
                      value={closeGapReason}
                      onChange={(e) => setCloseGapReason(e.target.value)}
                      placeholder="闭合理由（必填，写审计链）"
                    />
                    <Button size="sm" className="h-7 text-xs" onClick={() => closeGap(g.id)}>
                      确认闭合
                    </Button>
                    <Button variant="ghost" size="sm" className="h-7 text-xs" onClick={() => setCloseGapID(null)}>
                      取消
                    </Button>
                  </div>
                )}
              </div>
            ))}
          </div>
        </div>
      )}

      {/* 意图冲突仲裁（0.14.2）：检测确定性 + arbiter 调研判定；frozen=等人兜底 */}
      {conflicts.length > 0 && (
        <div className="rounded-lg border p-3">
          <div className="mb-2 flex items-center gap-2 font-medium text-sm">
            意图冲突
            <Badge variant="outline" className="text-[10px]">
              {conflicts.length}
            </Badge>
          </div>
          <div className="space-y-2">
            {conflicts.map((c) => (
              <div key={c.id} className="rounded-md border px-2 py-1.5">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="font-mono text-xs">#{c.id}</span>
                  <Badge variant="outline" className="text-[10px]">
                    {c.detect_rule}
                  </Badge>
                  <Badge variant={c.state === "frozen" ? "destructive" : "secondary"} className="text-[10px]">
                    {c.state}
                  </Badge>
                  {c.verdict && (
                    <Badge variant="outline" className="text-[10px]">
                      {c.verdict} · {c.resolved_by || "—"}
                    </Badge>
                  )}
                  {c.reopen_count > 0 && (
                    <span className="text-muted-foreground text-[10px]">重开×{c.reopen_count}</span>
                  )}
                  {(c.state === "frozen" || c.state === "adjudicated") && reopenID !== c.id && (
                    <Button
                      variant="outline"
                      size="sm"
                      className="ml-auto h-6 text-[10px]"
                      onClick={() => {
                        setReopenID(c.id);
                        setReopenReason("");
                      }}
                    >
                      重开
                    </Button>
                  )}
                </div>
                {c.verdict_reason && <div className="text-muted-foreground mt-1 text-xs">{c.verdict_reason}</div>}
                {reopenID === c.id && (
                  <div className="mt-1.5 flex flex-wrap items-center gap-1.5">
                    <input
                      className="h-7 w-72 rounded-md border bg-background px-2 text-xs"
                      value={reopenReason}
                      onChange={(e) => setReopenReason(e.target.value)}
                      placeholder="重开理由（必填，写审计链）"
                    />
                    <Button size="sm" className="h-7 text-xs" onClick={() => reopenConflict(c.id)}>
                      确认重开
                    </Button>
                    <Button variant="ghost" size="sm" className="h-7 text-xs" onClick={() => setReopenID(null)}>
                      取消
                    </Button>
                  </div>
                )}
              </div>
            ))}
          </div>
        </div>
      )}

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

      {/* 宿主授权集合（桌面控制闸门第五分支的判定依据，D9） */}
      <div className="rounded-lg border p-3">
        <div className="mb-1 font-medium text-sm">宿主授权集合（桌面控制）</div>
        <div className="text-muted-foreground text-xs">桌面动作授权清单（空=拒绝）</div>
        <div className="mt-2 flex flex-wrap items-center gap-2">
          <input
            className="h-8 flex-1 rounded-md border bg-background px-2 text-xs"
            value={hostInput}
            onChange={(e) => {
              setHostInput(e.target.value);
              setHostDirty(true);
            }}
            placeholder="逗号分隔，如：app:微信, win:订单系统"
          />
          <Button
            size="sm"
            className="h-8 text-xs"
            disabled={!hostDirty}
            onClick={async () => {
              try {
                await api.taskSetHostTargets(
                  taskId,
                  hostInput
                    .split(",")
                    .map((s) => s.trim())
                    .filter(Boolean),
                );
                toast.success("宿主授权集合已更新（动作进审计链）");
                setHostDirty(false);
              } catch (err) {
                toast.error(`保存失败：${String(err)}`);
              }
            }}
          >
            保存
          </Button>
        </div>
        {hostTargets.length > 0 && (
          <div className="mt-1 flex flex-wrap gap-1">
            {hostTargets.map((t) => (
              <Badge key={t} variant="outline" className="text-[10px]">
                {t}
              </Badge>
            ))}
          </div>
        )}
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
