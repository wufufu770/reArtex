"use client";

// 偏锋探索（0.15.4，按用户反馈三改）：
//   · 每一条方法论（内置与自定义）都可点开全量编辑——名称/档位/证据级/判据
//     类型与上限/说明/标签/优先级，走同一套注册期校验；内置条目保存为覆盖
//     配置（builtin-override），「恢复默认」随时回退代码常量本体；
//   · 后端配置写穿运行时注册表：启停/优先级/本体改动立即进入轮换与判定
//     （0.15.3 前只改展示，轮换读不到——这是"功能是虚的"的最后一处根因）；
//   · 方法论行可点开看完整内容；轮换优先级就地自动保存；运行记录展示真实
//     数据并可跳任务详情（0.15.3 既定）。
// 视觉纪律延续：无标语/引导文字；文字只作为数据出现。

import * as React from "react";

import Link from "next/link";
import { ChevronDownIcon, PlusIcon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { api } from "@/lib/api";
import type { EdgeMethodology, EdgeTaskOverview } from "@/lib/types";

type MethRow = EdgeMethodology;

const GRADE_DOT: Record<string, string> = {
  empirical: "bg-emerald-500",
  standard: "bg-sky-500",
  metaphor: "bg-purple-500",
};

const GRADE_NAME: Record<string, string> = {
  empirical: "实证",
  standard: "标准",
  metaphor: "隐喻",
};

const TIER_NAME: Record<number, string> = {
  1: "一级·机器可判",
  2: "二级·需改造",
  3: "三级·隐喻",
};

const CRIT_NAME = {
  coverage: "覆盖",
  convergence: "收敛",
  exhaust: "穷举",
  deplete: "耗尽",
  decay: "衰减",
} as const;

const TASK_STATUS: Record<string, { label: string; cls: string }> = {
  running: { label: "进行中", cls: "border-transparent bg-emerald-500/15 text-emerald-500" },
  pending: { label: "排队中", cls: "" },
  completed: { label: "已完成", cls: "text-muted-foreground" },
  stopped: { label: "已停止", cls: "text-muted-foreground" },
  failed: { label: "失败", cls: "border-transparent bg-destructive/15 text-destructive" },
};

// 自定义条目的编辑草稿（新增表单与展开编辑共用同一形状；grade/kind 收敛为
// 平台枚举字面量，payload 构造处不再需要宽断言）。
interface MethDraft {
  id: string;
  name: string;
  summary: string;
  tier: string;
  grade: EdgeMethodology["evidence_grade"];
  kind: keyof typeof CRIT_NAME;
  limit: string;
  tags: string;
  priority: string;
}

// 档位联动：隐喻档不设判据，证据级随档位归位（3↔metaphor）。
const gradeOnTier = (tier: string, cur: MethDraft["grade"]): MethDraft["grade"] =>
  tier === "3" ? "metaphor" : cur === "metaphor" ? "standard" : cur;

const draftOf = (m: MethRow): MethDraft => ({
  id: m.id,
  name: m.name,
  summary: m.summary,
  tier: String(m.tier),
  grade: m.evidence_grade,
  kind: (m.criterion?.kind ?? "coverage") as keyof typeof CRIT_NAME,
  limit: String(m.criterion?.bounds?.[0]?.limit ?? 10),
  tags: (m.heuristic_tags ?? []).join(", "),
  priority: String(m.priority),
});

const EMPTY_DRAFT: MethDraft = {
  id: "",
  name: "",
  summary: "",
  tier: "2",
  grade: "standard",
  kind: "coverage",
  limit: "10",
  tags: "",
  priority: "0",
};

// PUT payload：自定义=全量；内置行服务端会以代码常量覆盖本体字段（只收
// enabled/priority），所以统一发全量是安全的。
const payloadOf = (m: MethRow, over: Partial<MethRow> = {}) => ({
  name: m.name,
  tier: m.tier,
  evidence_grade: m.evidence_grade,
  summary: m.summary,
  heuristic_tags: m.heuristic_tags ?? [],
  criterion: m.criterion,
  enabled: m.enabled,
  priority: m.priority,
  ...over,
});

export default function PianfengPage() {
  const [meths, setMeths] = React.useState<MethRow[]>([]);
  const [tasks, setTasks] = React.useState<EdgeTaskOverview[]>([]);

  // 发起表单。
  const [goal, setGoal] = React.useState("");
  const [desc, setDesc] = React.useState("");
  const [meth, setMeth] = React.useState("");
  const [starting, setStarting] = React.useState(false);

  // 展开（手风琴：一次一条）+ 自定义条目编辑草稿。
  const [expanded, setExpanded] = React.useState<string | null>(null);
  const [draft, setDraft] = React.useState<MethDraft | null>(null);

  // 新增自定义表单。
  const [adding, setAdding] = React.useState(false);
  const [nf, setNf] = React.useState<MethDraft>(EMPTY_DRAFT);

  const load = React.useCallback(() => {
    Promise.all([api.edgeMethodologies().catch(() => null), api.edgeTasks().catch(() => null)]).then(
      ([ms, ts]) => {
        setMeths((ms?.methodologies ?? []) as MethRow[]);
        setTasks(ts?.tasks ?? []);
      },
    );
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  // 有进行中的运行时轻轮询，让判据进度活起来。
  const hasOpen = tasks.some((t) => t.open_run);
  React.useEffect(() => {
    if (!hasOpen) return;
    const h = setInterval(() => void load(), 5000);
    return () => clearInterval(h);
  }, [hasOpen, load]);

  const selectedM = meths.find((m) => m.id === meth && m.enabled);

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
      setMeth("");
      load();
    } catch (e) {
      toast.error(`发起失败：${(e as Error).message}`);
    } finally {
      setStarting(false);
    }
  };

  const setPrio = async (m: MethRow, v: number) => {
    if (!Number.isFinite(v) || v === m.priority) return;
    try {
      await api.edgeMethodologyPut(m.id, payloadOf(m, { priority: v }));
      toast.success(`${m.name}：轮换优先级已改为 ${v}`);
      load();
    } catch (e) {
      toast.error(`保存失败：${(e as Error).message}`);
      load();
    }
  };

  const toggleEnabled = async (m: MethRow) => {
    try {
      await api.edgeMethodologyPut(m.id, payloadOf(m, { enabled: !m.enabled }));
      load();
    } catch (e) {
      toast.error(`启停失败：${(e as Error).message}`);
    }
  };

  // 内置覆盖行 → 恢复默认；自定义行 → 删除。
  const removeOrRestore = async (m: MethRow) => {
    try {
      const r = await api.edgeMethodologyDelete(m.id);
      toast.success(r.restored_default ? `${m.name} 已恢复默认` : `${m.name} 已删除`);
      if (expanded === m.id) {
        setExpanded(null);
        setDraft(null);
      }
      load();
    } catch (e) {
      toast.error(`操作失败：${(e as Error).message}`);
    }
  };

  const openRow = (m: MethRow) => {
    if (expanded === m.id) {
      setExpanded(null);
      setDraft(null);
      return;
    }
    setExpanded(m.id);
    setDraft(draftOf(m)); // 所有条目统一全量编辑（内置=覆盖配置，恢复默认可回退）
  };

  const draftPayload = (d: MethDraft, enabled: boolean) => {
    const tier = Number(d.tier);
    const limit = Number(d.limit);
    const id = d.id.trim();
    if (!id) return { error: "id 必填" as const };
    if (!d.name.trim()) return { error: "名称必填" as const };
    if (tier !== 3 && (!Number.isFinite(limit) || limit <= 0)) return { error: "判据上限需为正数" as const };
    return {
      body: {
        name: d.name.trim(),
        tier,
        evidence_grade: d.grade,
        summary: d.summary.trim(),
        heuristic_tags: d.tags
          .split(/[,，]/)
          .map((s) => s.trim().replace(/^#/, ""))
          .filter(Boolean),
        criterion:
          tier === 3
            ? undefined
            : { kind: d.kind, bounds: [{ kind: d.kind === "decay" ? "decay" : "target_set", limit }] },
        enabled,
        priority: Number(d.priority) || 0,
      },
      id,
    };
  };

  const saveDraft = async (m: MethRow) => {
    if (!draft) return;
    const p = draftPayload(draft, m.enabled);
    if (p.error) {
      toast.error(p.error);
      return;
    }
    try {
      await api.edgeMethodologyPut(p.id, p.body);
      toast.success("已保存");
      setExpanded(null);
      setDraft(null);
      load();
    } catch (e) {
      // 全量编辑走注册期同套校验，后端会给出具体拒绝原因。
      toast.error(`保存被拒：${(e as Error).message}`);
    }
  };

  const addCustom = async () => {
    const p = draftPayload(nf, true);
    if (p.error) {
      toast.error(p.error);
      return;
    }
    try {
      await api.edgeMethodologyPut(p.id, p.body);
      toast.success(`自定义方法论 ${p.id} 已添加`);
      setAdding(false);
      setNf(EMPTY_DRAFT);
      load();
    } catch (e) {
      toast.error(`添加被拒：${(e as Error).message}`);
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

  const tierSelect = (v: string, on: (v: string) => void) => (
    <select className="h-7 rounded-md border bg-background px-1 text-xs" value={v} onChange={(e) => on(e.target.value)}>
      <option value="1">一级·机器可判</option>
      <option value="2">二级·需改造</option>
      <option value="3">三级·隐喻</option>
    </select>
  );

  const gradeSelect = (v: string, on: (v: string) => void) => (
    <select className="h-7 rounded-md border bg-background px-1 text-xs" value={v} onChange={(e) => on(e.target.value)}>
      <option value="standard">标准</option>
      <option value="empirical">实证</option>
      <option value="metaphor">隐喻</option>
    </select>
  );

  const kindSelect = (v: string, on: (v: string) => void) => (
    <select className="h-7 rounded-md border bg-background px-1 text-xs" value={v} onChange={(e) => on(e.target.value)}>
      {Object.entries(CRIT_NAME).map(([k, name]) => (
        <option key={k} value={k}>
          {name}
        </option>
      ))}
    </select>
  );

  return (
    <div className="flex flex-col gap-4 p-4 md:p-6">
      <h1 className="font-semibold text-xl tracking-tight">偏锋探索</h1>

      {/* ① 发起（直接布置任务，与 edge_run 工具同宿主链） */}
      <div className="rounded-lg border p-3">
        <div className="flex flex-wrap items-center gap-2">
          <Input
            className="h-8 min-w-48 flex-1 text-sm"
            placeholder="任务目标（一句话）"
            value={goal}
            onChange={(e) => setGoal(e.target.value)}
          />
          <Input
            className="h-8 min-w-48 flex-1 text-sm"
            placeholder="范围说明（必填）"
            value={desc}
            onChange={(e) => setDesc(e.target.value)}
          />
          <select
            className="h-8 rounded-md border bg-background px-2 text-sm"
            value={meth}
            onChange={(e) => setMeth(e.target.value)}
          >
            <option value="">自动轮换</option>
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
        {selectedM && <p className="mt-1.5 text-muted-foreground text-xs">{selectedM.summary}</p>}
      </div>

      {/* ② 运行记录（真实数据：目标/状态/判据进度/运行态；可跳详情、可停止） */}
      {tasks.length > 0 && (
        <div className="rounded-lg border p-3">
          <div className="mb-2 flex items-center gap-2 font-medium text-sm">
            运行记录
            <Badge variant="outline" className="text-[10px]">
              {tasks.length}
            </Badge>
          </div>
          <div className="space-y-1.5">
            {tasks.map((t) => {
              const st = TASK_STATUS[t.status] ?? { label: t.status, cls: "" };
              const stopHistory = Object.entries(t.stop_stats ?? {})
                .filter(([, v]) => v > 0)
                .map(([k, v]) => `${k}×${v}`)
                .join(" · ");
              return (
                <div key={t.task_id} className="flex flex-wrap items-center gap-2 rounded-md border px-2 py-1.5">
                  <span className="font-mono text-xs">#{t.task_id}</span>
                  <Badge variant="outline" className={`text-[10px] ${st.cls}`}>
                    {st.label}
                  </Badge>
                  <span className="min-w-0 flex-1 truncate text-sm" title={t.task_desc || t.task_goal}>
                    {t.task_goal || t.task_desc || "—"}
                  </span>
                  <span className="text-[10px] text-muted-foreground">{t.methodology_id || "轮换"}</span>
                  <span className="text-[10px] text-muted-foreground">
                    {t.cells_terminal}/{t.cells_total} 格
                  </span>
                  <div className="h-1 w-16 shrink-0 overflow-hidden rounded bg-muted">
                    <div
                      className="h-full bg-primary"
                      style={{ width: `${Math.min(100, Math.round(t.criterion_progress * 100))}%` }}
                    />
                  </div>
                  {stopHistory && <span className="text-[10px] text-muted-foreground">{stopHistory}</span>}
                  <Link
                    href={`/function/tasks/detail?id=${t.task_id}`}
                    className="shrink-0 text-primary text-xs hover:underline"
                  >
                    查看
                  </Link>
                  {t.open_run && (
                    <Button variant="outline" size="sm" className="h-6 text-[10px]" onClick={() => stop(t)}>
                      停止
                    </Button>
                  )}
                </div>
              );
            })}
          </div>
        </div>
      )}

      {/* ③ 方法论（行=快捷操作；点开=完整内容+编辑） */}
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
          <div className="mb-3 space-y-1.5 rounded-md border p-2">
            <div className="flex flex-wrap items-center gap-2">
              <Input
                className="h-7 w-40 text-xs"
                placeholder="id（如 my-method）"
                value={nf.id}
                onChange={(e) => setNf({ ...nf, id: e.target.value })}
              />
              {tierSelect(nf.tier, (v) => setNf({ ...nf, tier: v, grade: gradeOnTier(v, nf.grade) }))}
              {gradeSelect(nf.grade, (v) => setNf({ ...nf, grade: v as MethDraft["grade"] }))}
              {Number(nf.tier) !== 3 && kindSelect(nf.kind, (v) => setNf({ ...nf, kind: v as MethDraft["kind"] }))}
              {Number(nf.tier) !== 3 && (
                <Input
                  className="h-7 w-20 text-xs"
                  placeholder="上限"
                  value={nf.limit}
                  onChange={(e) => setNf({ ...nf, limit: e.target.value })}
                />
              )}
              <Input
                className="h-7 min-w-40 flex-1 text-xs"
                placeholder="一句话说明"
                value={nf.summary}
                onChange={(e) => setNf({ ...nf, summary: e.target.value })}
              />
            </div>
            <div className="flex flex-wrap items-center gap-2">
              <Input
                className="h-7 min-w-40 flex-1 text-xs"
                placeholder="启发标签，逗号分隔"
                value={nf.tags}
                onChange={(e) => setNf({ ...nf, tags: e.target.value })}
              />
              <Input
                className="h-7 w-16 text-center text-xs"
                placeholder="优先级"
                title="轮换优先级：大者优先"
                value={nf.priority}
                onChange={(e) => setNf({ ...nf, priority: e.target.value })}
              />
              <Button size="sm" className="h-7 text-xs" onClick={addCustom}>
                添加
              </Button>
            </div>
          </div>
        )}

        <div className="space-y-1.5">
          {meths.map((m) => {
            const open = expanded === m.id;
            return (
              <div key={m.id} className={`rounded-md border ${open ? "bg-muted/30" : ""}`}>
                {/* 行：点开看完整内容；右侧为快捷操作（优先级/启停/删） */}
                <div
                  className="flex cursor-pointer select-none flex-wrap items-center gap-2 px-2 py-1.5"
                  onClick={() => openRow(m)}
                >
                  <ChevronDownIcon
                    className={`size-3.5 shrink-0 text-muted-foreground transition-transform ${open ? "rotate-180" : ""}`}
                  />
                  <span className={`size-2 shrink-0 rounded-full ${GRADE_DOT[m.evidence_grade] ?? "bg-slate-400"}`} />
                  <span className="font-medium text-sm">{m.name}</span>
                  <span className="text-[10px] text-muted-foreground">T{m.tier}</span>
                  {m.criterion && (
                    <span className="text-[10px] text-muted-foreground">
                      {(CRIT_NAME as Record<string, string>)[m.criterion.kind] ?? m.criterion.kind}
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
                  <div className="ml-auto flex items-center gap-1.5" onClick={(e) => e.stopPropagation()}>
                    <input
                      className="h-6 w-12 rounded border bg-background px-1 text-center text-[10px]"
                      defaultValue={m.priority}
                      title="轮换优先级：大者优先"
                      onBlur={(e) => setPrio(m, Number(e.target.value))}
                      onKeyDown={(e) => {
                        if (e.key === "Enter") (e.target as HTMLInputElement).blur();
                      }}
                    />
                    <Button variant="ghost" size="sm" className="h-6 text-[10px]" onClick={() => toggleEnabled(m)}>
                      {m.enabled ? "停用" : "启用"}
                    </Button>
                    {!m.enabled && m.source !== "builtin" && (
                      <Button
                        variant="ghost"
                        size="sm"
                        className="h-6 text-[10px] text-destructive"
                        onClick={() => removeOrRestore(m)}
                      >
                        {m.source === "builtin-override" ? "恢复默认" : "删除"}
                      </Button>
                    )}
                  </div>
                </div>

                {/* 展开：完整内容 + 编辑区 */}
                {open && (
                  <div className="border-t px-3 py-2.5" onClick={(e) => e.stopPropagation()}>
                    <p className="text-sm">{m.summary || "—"}</p>
                    <div className="mt-1.5 flex flex-wrap gap-x-3 text-[11px] text-muted-foreground">
                      <span>{TIER_NAME[m.tier] ?? `T${m.tier}`}</span>
                      <span>{GRADE_NAME[m.evidence_grade] ?? m.evidence_grade}证据</span>
                      <span>轮换优先级 {m.priority}（大者优先）</span>
                    </div>
                    <div className="mt-1 text-[11px] text-muted-foreground">
                      {m.tier === 3
                        ? "隐喻条目：不设机器判据，仅作视角注入"
                        : `完成判据：${(CRIT_NAME as Record<string, string>)[m.criterion?.kind ?? ""] ?? "—"}${
                            m.criterion?.bounds?.[0] ? ` ≤ ${m.criterion.bounds[0].limit}` : ""
                          }`}
                    </div>
                    {(m.heuristic_tags ?? []).length > 0 && (
                      <div className="mt-1.5 flex flex-wrap gap-1.5">
                        {(m.heuristic_tags ?? []).map((tag) => (
                          <Badge key={tag} variant="outline" className="text-[10px]">
                            #{tag}
                          </Badge>
                        ))}
                      </div>
                    )}

                    {draft && (
                      <div className="mt-2.5 space-y-1.5">
                        <div className="flex flex-wrap items-center gap-2">
                          <Input
                            className="h-7 w-40 text-xs"
                            placeholder="名称"
                            value={draft.name}
                            onChange={(e) => setDraft({ ...draft, name: e.target.value })}
                          />
                          {tierSelect(draft.tier, (v) => setDraft({ ...draft, tier: v, grade: gradeOnTier(v, draft.grade) }))}
                          {gradeSelect(draft.grade, (v) => setDraft({ ...draft, grade: v as MethDraft["grade"] }))}
                          {Number(draft.tier) !== 3 &&
                            kindSelect(draft.kind, (v) => setDraft({ ...draft, kind: v as MethDraft["kind"] }))}
                          {Number(draft.tier) !== 3 && (
                            <Input
                              className="h-7 w-20 text-xs"
                              placeholder="上限"
                              value={draft.limit}
                              onChange={(e) => setDraft({ ...draft, limit: e.target.value })}
                            />
                          )}
                        </div>
                        <div className="flex flex-wrap items-center gap-2">
                          <Input
                            className="h-7 min-w-40 flex-1 text-xs"
                            placeholder="一句话说明"
                            value={draft.summary}
                            onChange={(e) => setDraft({ ...draft, summary: e.target.value })}
                          />
                          <Input
                            className="h-7 min-w-40 flex-1 text-xs"
                            placeholder="启发标签，逗号分隔"
                            value={draft.tags}
                            onChange={(e) => setDraft({ ...draft, tags: e.target.value })}
                          />
                          <Input
                            className="h-7 w-16 text-center text-xs"
                            title="轮换优先级：大者优先"
                            value={draft.priority}
                            onChange={(e) => setDraft({ ...draft, priority: e.target.value })}
                          />
                          <Button size="sm" className="h-7 text-xs" onClick={() => saveDraft(m)}>
                            保存
                          </Button>
                          {m.source === "builtin-override" && (
                            <Button variant="ghost" size="sm" className="h-7 text-xs" onClick={() => removeOrRestore(m)}>
                              恢复默认
                            </Button>
                          )}
                          {m.source === "custom" && !m.enabled && (
                            <Button
                              variant="ghost"
                              size="sm"
                              className="h-7 text-destructive text-xs"
                              onClick={() => removeOrRestore(m)}
                            >
                              删除
                            </Button>
                          )}
                        </div>
                        {m.source === "builtin" && (
                          <div className="text-[11px] text-muted-foreground">
                            保存后将存为覆盖配置，可随时恢复默认。
                          </div>
                        )}
                      </div>
                    )}
                  </div>
                )}
              </div>
            );
          })}
        </div>
      </div>
    </div>
  );
}
