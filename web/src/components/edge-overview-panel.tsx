"use client";

// 偏锋探索总览面板（EDGE 总览；0.11.8 重构：并入业务建模页作为 edge 模式，
// 不再是独立静态路由——独立路由是这个功能点唯一与其它可达页面的结构差异）。
//
// 完整入口口径：
//   - 发起：对话页选「偏锋探索」agent，或对主 agent 说「这一轮用偏锋探索」
//   - 本面板：只读总览——方法论注册表 + 全部偏锋任务的判据进度与停止原因统计
//   - sidebar「偏锋探索」→ /function/business-graph?mode=edge

import * as React from "react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { api } from "@/lib/api";
import type { EdgeMethodology, EdgeTaskOverview } from "@/lib/types";

const TIER_LABEL: Record<number, string> = { 1: "一级·机器可判", 2: "二级·需改造", 3: "三级·隐喻" };
const GRADE_LABEL: Record<string, string> = { empirical: "实证", standard: "标准", metaphor: "隐喻" };
const GRADE_BADGE: Record<string, string> = { empirical: "default", standard: "secondary", metaphor: "outline" };

const STOP_LABEL: Record<string, string> = {
  doctrine_complete: "判据成立",
  degenerate_hypothesis: "假设退化熔断",
  criterion_stalled: "判据停滞熔断",
  cell_exhausted: "格点封顶熔断",
  operator_guard: "人设兜底",
  cancelled: "人工取消",
  failed: "失败",
  "": "进行中",
};

function CriterionBadges({ m }: { m: EdgeMethodology }) {
  if (!m.criterion) {
    return (
      <Badge variant="outline" className="text-[10px]">
        无判据（隐喻只有启发式）
      </Badge>
    );
  }
  return (
    <>
      <Badge variant="secondary" className="text-[10px]">
        判据 {m.criterion.kind}
      </Badge>
      {m.criterion.bounds.map((b) => (
        <Badge key={b.kind} variant="outline" className="text-[10px]">
          {b.kind}≤{b.limit}
        </Badge>
      ))}
    </>
  );
}

export function EdgeOverviewPanel() {
  const [methodologies, setMethodologies] = React.useState<EdgeMethodology[]>([]);
  const [tasks, setTasks] = React.useState<EdgeTaskOverview[]>([]);
  const [loading, setLoading] = React.useState(false);

  const load = React.useCallback(() => {
    setLoading(true);
    Promise.all([api.edgeMethodologies(), api.edgeTasks()])
      .then(([ms, ts]) => {
        setMethodologies(ms.methodologies ?? []);
        setTasks(ts.tasks ?? []);
      })
      .catch((err) => console.error("[edge-overview]", err))
      .finally(() => setLoading(false));
  }, []);

  React.useEffect(() => {
    load();
  }, [load]);

  return (
    <div className="space-y-4">
      <div className="flex items-center gap-2">
        <span className="text-xs text-muted-foreground">
          方法论驱动的另一条探索线——发起走对话（选「偏锋探索」agent，或对主 agent
          说「这一轮用偏锋探索」）；本面板是只读总览。
        </span>
        <Button variant="outline" size="sm" className="ml-auto h-8 text-xs" onClick={load} disabled={loading}>
          {loading ? "加载中…" : "刷新"}
        </Button>
      </div>

      {/* 偏锋任务与判据进度 */}
      <div className="space-y-2">
        <div className="text-sm font-medium">偏锋任务与判据进度</div>
        {tasks.length === 0 && (
          <div className="rounded-lg border p-6 text-center text-sm text-muted-foreground">
            还没有偏锋任务。到「对话」页选「偏锋探索」agent 说「做一次完整探索」，或在任务详情对话里对主 agent
            说「这一轮用偏锋探索」。
          </div>
        )}
        {tasks.map((t) => (
          <div key={t.task_id} className="rounded-lg border p-3">
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-sm font-medium">任务 #{t.task_id}</span>
              <Badge variant="outline" className="text-[10px]">
                {t.methodology_id || "—"}
              </Badge>
              <Badge variant={t.status === "running" ? "default" : "secondary"} className="text-[10px]">
                {t.status}
              </Badge>
              {t.open_run && <Badge className="text-[10px]">运行中 #{t.open_run.id}</Badge>}
              <span className="ml-auto text-xs text-muted-foreground">
                判据进度 {t.cells_terminal}/{t.cells_total}（{Math.round((t.criterion_progress || 0) * 100)}%）
              </span>
            </div>
            <div className="mt-1 line-clamp-2 text-xs text-muted-foreground">{t.task_desc || t.task_goal}</div>
            <div className="mt-2 h-1.5 w-full overflow-hidden rounded-full bg-muted">
              <div
                className="h-full rounded-full bg-primary transition-all"
                style={{ width: `${Math.round((t.criterion_progress || 0) * 100)}%` }}
              />
            </div>
            {t.stop_stats && Object.keys(t.stop_stats).length > 0 && (
              <div className="mt-2 flex flex-wrap gap-1">
                {Object.entries(t.stop_stats).map(([reason, n]) => (
                  <Badge key={reason || "running"} variant="outline" className="text-[10px]">
                    {STOP_LABEL[reason] ?? reason} ×{n}
                  </Badge>
                ))}
              </div>
            )}
          </div>
        ))}
      </div>

      {/* 方法论注册表 */}
      <div className="space-y-2">
        <div className="text-sm font-medium">方法论注册表</div>
        <div className="grid gap-2 md:grid-cols-2">
          {methodologies.map((m) => (
            <div key={m.id} className={`rounded-lg border p-3 ${m.enabled ? "" : "opacity-50"}`}>
              <div className="flex flex-wrap items-center gap-2">
                <span className="text-sm font-medium">{m.name}</span>
                <Badge variant="outline" className="text-[10px]">
                  {TIER_LABEL[m.tier]}
                </Badge>
                <Badge
                  variant={(GRADE_BADGE[m.evidence_grade] ?? "outline") as "default" | "secondary" | "outline"}
                  className="text-[10px]"
                >
                  {GRADE_LABEL[m.evidence_grade] ?? m.evidence_grade}
                </Badge>
                {!m.enabled && (
                  <Badge variant="outline" className="text-[10px]">
                    已停用
                  </Badge>
                )}
              </div>
              <p className="mt-1 text-xs text-muted-foreground">{m.summary}</p>
              <div className="mt-2 flex flex-wrap gap-1">
                <CriterionBadges m={m} />
              </div>
              {m.heuristic_tags.length > 0 && (
                <div className="mt-1 flex flex-wrap gap-1">
                  {m.heuristic_tags.map((tag) => (
                    <Badge key={tag} variant="outline" className="text-[10px]">
                      启发式：{tag}
                    </Badge>
                  ))}
                </div>
              )}
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
