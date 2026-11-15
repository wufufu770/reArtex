"use client";

// 偏锋探索（0.15.2 重写，按用户裁定）：目标/架构/设计/实现从简——
//   ① 发起：任务目标 + 方法论 → 直接布置（与 edge_run 工具同宿主链）；
//   ② 运行中：判据进度 + 人工停止；
//   ③ 方法论：紧凑行 + 启停/优先级/删（0.14.7 的 PUT/DELETE 落地到 UI）
//      + 新增自定义（过平台同一套注册期校验）。
// 视觉纪律：无标语/提示文字/次要徽章；summary 折进 title 悬停；tier/grade 用
// 色点；隐喻不显示"无判据"。

import * as React from "react";

import { PlusIcon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { api } from "@/lib/api";
import type { EdgeMethodology, EdgeTaskOverview } from "@/lib/types";

const GRADE_DOT: Record<string, string> = {
  empirical: "bg-emerald-500",
  standard: "bg-sky-500",
  metaphor: "bg-purple-500",
};

const CRIT_SHORT: Record<string, string> = {
  coverage: "覆盖",
  convergence: "收敛",
  exhaust: "穷举",
  deplete: "耗尽",
  decay: "衰减",
};

interface MethRow extends EdgeMethodology {
  prioDraft?: number;
}

export default function PianfengPage() {
  const [meths, setMeths] = React.useState<MethRow[]>([]);
  const [tasks, setTasks] = React.useState<EdgeTaskOverview[]>([]);
  const [, setLoading] = React.useState(false);

  // 发起表单。
  const [goal, setGoal] = React.useState("");
  const [desc, setDesc] = React.useState("");
  const [meth, setMeth] = React.useState("");
  const [starting, setStarting] = React.useState(false);

  // 新增自定义表单。
  const [adding, setAdding] = React.useState(false);
  const [nf, setNf] = React.useState({
    id: "",
    name: "",
    tier: "2",
    grade: "standard",
    summary: "",
    kind: "coverage",
    limit: "10",
  });

  const load = React.useCallback(() => {
    setLoading(true);
    Promise.all([api.edgeMethodologies().catch(() => null), api.edgeTasks().catch(() => null)])
      .then(([ms, ts]) => {
        setMeths((ms?.methodologies ?? []) as MethRow[]);
        setTasks(ts?.tasks ?? []);
      })
      .finally(() => setLoading(false));
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  const start = async () => {
    if (!goal.trim() || !desc.trim()) {
      toast.error("任务目标与范围说明必填");
      return;
    }
    setStarting(true);
    try {
      const r = await api.edgeRun(goal.trim(), desc.trim(), meth || undefined);
      toast.success(`偏锋任务 #${r.task_id} 已发起，执行体已排队`);
      setGoal("");
      setDesc("");
      load();
    } catch (e) {
      toast.error(`发起失败：${(e as Error).message}`);
    } finally {
      setStarting(false);
    }
  };

  const saveMeth = async (m: MethRow) => {
    try {
      await api.edgeMethodologyPut(m.id, {
        name: m.name,
        tier: m.tier,
        evidence_grade: m.evidence_grade,
        summary: m.summary,
        heuristic_tags: m.heuristic_tags ?? [],
        criterion: m.criterion,
        enabled: m.enabled,
        priority: m.prioDraft ?? m.priority,
      });
      toast.success(`${m.name || m.id} 已保存`);
      load();
    } catch (e) {
      toast.error(`保存失败：${(e as Error).message}`);
    }
  };

  const toggleEnabled = async (m: MethRow) => {
    try {
      await api.edgeMethodologyPut(m.id, {
        name: m.name,
        tier: m.tier,
        evidence_grade: m.evidence_grade,
        summary: m.summary,
        heuristic_tags: m.heuristic_tags ?? [],
        criterion: m.criterion,
        enabled: !m.enabled,
        priority: m.priority,
      });
      load();
    } catch (e) {
      toast.error(`启停失败：${(e as Error).message}`);
    }
  };

  const removeMeth = async (m: MethRow) => {
    try {
      const r = await api.edgeMethodologyDelete(m.id);
      toast.success(r.restored_default ? `${m.name || m.id} 已恢复默认` : `${m.name || m.id} 已删除`);
      load();
    } catch (e) {
      toast.error(`删除失败：${(e as Error).message}`);
    }
  };

  const addCustom = async () => {
    try {
      const r = await api.edgeMethodologyImport([
        {
          id: nf.id.trim(),
          name: nf.name.trim(),
          tier: Number(nf.tier),
          evidence_grade: nf.grade,
          summary: nf.summary.trim(),
          criterion:
            Number(nf.tier) === 3
              ? undefined
              : {
                  kind: nf.kind,
                  bounds: [{ kind: nf.kind === "decay" ? "decay" : "target_set", limit: Number(nf.limit) }],
                },
          enabled: true,
          priority: 0,
        },
      ]);
      if (r.rejected.length > 0) {
        toast.error(`校验拒绝：${nf.id}（判据/上限/隐喻规则不满足）`);
        return;
      }
      toast.success(`自定义方法论 ${nf.id} 已添加`);
      setAdding(false);
      setNf({ id: "", name: "", tier: "2", grade: "standard", summary: "", kind: "coverage", limit: "10" });
      load();
    } catch (e) {
      toast.error(`添加失败：${(e as Error).message}`);
    }
  };

  const stop = async (t: EdgeTaskOverview) => {
    try {
      await api.edgeStop(t.task_id);
      toast.success(`任务 #${t.task_id} 已停止`);
      load();
    } catch (e) {
      toast.error(`停止失败：${(e as Error).message}`);
    }
  };

  return (
    <div className="flex flex-col gap-4 p-4 md:p-6">
      <h1 className="font-semibold text-xl tracking-tight">偏锋探索</h1>

      {/* ① 发起（直接布置任务） */}
      <div className="rounded-lg border p-3">
        <div className="mb-2 font-medium text-sm">发起偏锋探索</div>
        <div className="flex flex-wrap items-center gap-2">
          <Input
            className="h-8 w-64 text-sm"
            placeholder="任务目标（一句话）"
            value={goal}
            onChange={(e) => setGoal(e.target.value)}
          />
          <Input
            className="h-8 w-64 text-sm"
            placeholder="范围说明（必填）"
            value={desc}
            onChange={(e) => setDesc(e.target.value)}
          />
          <select
            className="h-8 rounded-md border bg-background px-2 text-sm"
            value={meth}
            onChange={(e) => setMeth(e.target.value)}
          >
            <option value="">轮换表首位</option>
            {meths
              .filter((m) => m.enabled)
              .map((m) => (
                <option key={m.id} value={m.id}>
                  {m.name}
                </option>
              ))}
          </select>
          <Button size="sm" className="h-8 text-xs" disabled={starting || !goal.trim() || !desc.trim()} onClick={start}>
            {starting ? "发起中…" : "发起"}
          </Button>
        </div>
      </div>

      {/* ② 运行中（判据进度 + 停止） */}
      {tasks.length > 0 && (
        <div className="rounded-lg border p-3">
          <div className="mb-2 font-medium text-sm">运行记录</div>
          <div className="space-y-1.5">
            {tasks.map((t) => (
              <div key={t.task_id} className="flex flex-wrap items-center gap-2 rounded-md border px-2 py-1.5">
                <span className="font-mono text-xs">#{t.task_id}</span>
                <Badge variant="outline" className="text-[10px]">
                  {t.methodology_id || "—"}
                </Badge>
                {Object.entries(t.stop_stats ?? {}).map(([k, v]) => (
                  <span key={k} className="text-[10px] text-muted-foreground">
                    {k}×{v}
                  </span>
                ))}
                <Button variant="outline" size="sm" className="ml-auto h-6 text-[10px]" onClick={() => stop(t)}>
                  停止
                </Button>
              </div>
            ))}
          </div>
        </div>
      )}

      {/* ③ 方法论（紧凑表：启停/优先级/删/新增） */}
      <div className="rounded-lg border p-3">
        <div className="mb-2 flex items-center gap-2 font-medium text-sm">
          方法论
          <Badge variant="outline" className="text-[10px]">
            {meths.length}
          </Badge>
          <Button variant="outline" size="sm" className="ml-auto h-7 text-xs" onClick={() => setAdding((v) => !v)}>
            <PlusIcon className="size-3.5" />
            新增
          </Button>
        </div>

        {adding && (
          <div className="mb-3 flex flex-wrap items-end gap-2 rounded-md border p-2">
            <Input
              className="h-7 w-36 text-xs"
              placeholder="id（如 my-method）"
              value={nf.id}
              onChange={(e) => setNf({ ...nf, id: e.target.value })}
            />
            <Input
              className="h-7 w-36 text-xs"
              placeholder="名称"
              value={nf.name}
              onChange={(e) => setNf({ ...nf, name: e.target.value })}
            />
            <select
              className="h-7 rounded-md border bg-background px-1 text-xs"
              value={nf.tier}
              onChange={(e) => setNf({ ...nf, tier: e.target.value })}
            >
              <option value="1">一级·机器可判</option>
              <option value="2">二级·需改造</option>
              <option value="3">三级·隐喻</option>
            </select>
            <select
              className="h-7 rounded-md border bg-background px-1 text-xs"
              value={nf.grade}
              onChange={(e) => setNf({ ...nf, grade: e.target.value })}
            >
              <option value="standard">标准</option>
              <option value="empirical">实证</option>
              <option value="metaphor">隐喻</option>
            </select>
            {Number(nf.tier) !== 3 && (
              <>
                <select
                  className="h-7 rounded-md border bg-background px-1 text-xs"
                  value={nf.kind}
                  onChange={(e) => setNf({ ...nf, kind: e.target.value })}
                >
                  {Object.entries(CRIT_SHORT).map(([k, v]) => (
                    <option key={k} value={k}>
                      {v}
                    </option>
                  ))}
                </select>
                <Input
                  className="h-7 w-20 text-xs"
                  placeholder="上限"
                  value={nf.limit}
                  onChange={(e) => setNf({ ...nf, limit: e.target.value })}
                />
              </>
            )}
            <Input
              className="h-7 w-56 text-xs"
              placeholder="一句话说明"
              value={nf.summary}
              onChange={(e) => setNf({ ...nf, summary: e.target.value })}
            />
            <Button size="sm" className="h-7 text-xs" onClick={addCustom}>
              添加
            </Button>
          </div>
        )}

        <div className="space-y-1.5">
          {meths.map((m) => (
            <div
              key={m.id}
              className="flex flex-wrap items-center gap-2 rounded-md border px-2 py-1.5"
              title={m.summary}
            >
              <span className={`size-2 shrink-0 rounded-full ${GRADE_DOT[m.evidence_grade] ?? "bg-slate-400"}`} />
              <span className="font-medium text-sm">{m.name}</span>
              <span className="text-[10px] text-muted-foreground">T{m.tier}</span>
              {m.criterion && (
                <span className="text-[10px] text-muted-foreground">
                  {CRIT_SHORT[m.criterion.kind] ?? m.criterion.kind}
                  {m.criterion.bounds?.[0] ? `≤${m.criterion.bounds[0].limit}` : ""}
                </span>
              )}
              {(m.heuristic_tags ?? []).slice(0, 3).map((tag) => (
                <span key={tag} className="text-[10px] text-muted-foreground">
                  #{tag}
                </span>
              ))}
              {!m.enabled && (
                <Badge variant="outline" className="text-[10px]">
                  停用
                </Badge>
              )}
              <div className="ml-auto flex items-center gap-1.5">
                <input
                  className="h-6 w-12 rounded border bg-background px-1 text-center text-[10px]"
                  value={m.prioDraft ?? m.priority}
                  onChange={(e) =>
                    setMeths((cur) => cur.map((x) => (x.id === m.id ? { ...x, prioDraft: Number(e.target.value) } : x)))
                  }
                  title="优先级"
                />
                <Button variant="outline" size="sm" className="h-6 text-[10px]" onClick={() => saveMeth(m)}>
                  保存
                </Button>
                <Button variant="ghost" size="sm" className="h-6 text-[10px]" onClick={() => toggleEnabled(m)}>
                  {m.enabled ? "停用" : "启用"}
                </Button>
                {!m.enabled && (
                  <Button
                    variant="ghost"
                    size="sm"
                    className="h-6 text-[10px] text-destructive"
                    onClick={() => removeMeth(m)}
                  >
                    删
                  </Button>
                )}
              </div>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
