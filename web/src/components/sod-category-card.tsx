"use client";

// v4.1 拦截种类卡（方案 §2/§11-G1）：种类 = 一类命令的名字 + 匹配器 + 动作 + 上限 + 兜底。
// 顶部是合并闸三态开关（off/shadow/enforce，enforce 需二次确认），下面是种类表（行内编辑）
// 与「新增 custom 种类」。锁定基线行（🔒）动作不可改、不可删——安全地板由后端强制，
// 这里照实禁用并把后端错误原文透传给 toast。

import * as React from "react";

import { LockIcon, PlusIcon, RefreshCwIcon, ShieldAlertIcon, Trash2Icon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Separator } from "@/components/ui/separator";
import { Switch } from "@/components/ui/switch";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { api } from "@/lib/api";
import type { SodCategory, SodCategoryAction, SodCategoryFallback, SodMergedGateMode } from "@/lib/types";

// ACTION_OPTIONS 是种类动作的中文标签（与后端 sodpolicy.CategoryAction* 一一对应）。
const ACTION_OPTIONS: { value: SodCategoryAction; label: string }[] = [
  { value: "hard_deny", label: "硬拒" },
  { value: "ask", label: "转人工" },
  { value: "auditor", label: "审计裁决" },
  { value: "pass", label: "放行" },
];

// FALLBACK_OPTIONS 是兜底动作的中文标签（auditor 故障/超时/超限时的落点）。
const FALLBACK_OPTIONS: { value: SodCategoryFallback; label: string }[] = [
  { value: "deny", label: "拒绝" },
  { value: "ask", label: "转人工" },
  { value: "pass", label: "放行" },
];

// MODE_LABEL 是合并闸三态的展示文案。
const MODE_LABEL: Record<SodMergedGateMode, string> = {
  off: "off · 旧链裁决",
  shadow: "shadow · 影子对照",
  enforce: "enforce · 新链接管",
};

// MODE_SUCCESS 是切换成功的播报文案。
const MODE_SUCCESS: Record<SodMergedGateMode, string> = {
  off: "已切回旧链裁决",
  shadow: "已进入影子对照：新旧链并跑，只记差异",
  enforce: "新链已接管裁决（可随时切回 off）",
};

// MergedGateBar 是合并闸三态开关条：读当前值 → 改 → 保存。
// enforce 是「接管裁决」级别的动作：先落一次确认提示，再点一次保存才真正提交，误触不了。
function MergedGateBar() {
  const [current, setCurrent] = React.useState<SodMergedGateMode>("off");
  const [mode, setMode] = React.useState<SodMergedGateMode>("off");
  const [operator, setOperator] = React.useState("sir");
  const [armed, setArmed] = React.useState(false);
  const [saving, setSaving] = React.useState(false);

  const load = React.useCallback(async () => {
    try {
      const m = await api.sodMergedGate();
      setCurrent(m);
      setMode(m);
    } catch (e) {
      toast.error(`读取合并闸状态失败：${(e as Error).message}`);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  function pick(v: string) {
    setMode(v as SodMergedGateMode);
    setArmed(false);
  }

  async function save() {
    if (!operator.trim()) {
      toast.error("先填操作者身份（审计留痕）");
      return;
    }
    if (mode === "enforce" && current !== "enforce" && !armed) {
      setArmed(true);
      return;
    }
    setSaving(true);
    try {
      await api.setSodMergedGate(mode, operator.trim());
      toast.success(MODE_SUCCESS[mode]);
      setArmed(false);
      await load();
    } catch (e) {
      toast.error(`保存失败：${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <div
      className={`flex flex-wrap items-end gap-3 rounded-md border px-3 py-3 ${
        mode === "enforce" ? "border-violet-400/50 bg-violet-50/40 dark:bg-violet-950/20" : "bg-muted/40"
      }`}
    >
      <div className="grid gap-1.5">
        <Label htmlFor="merged-gate-mode">合并闸</Label>
        <Select value={mode} onValueChange={pick}>
          <SelectTrigger id="merged-gate-mode" className="w-64">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="off">off · 旧链裁决（一键回退）</SelectItem>
            <SelectItem value="shadow">shadow · 新旧并跑只记差异</SelectItem>
            <SelectItem value="enforce">enforce · 新链接管裁决</SelectItem>
          </SelectContent>
        </Select>
      </div>
      <div className="grid gap-1.5">
        <Label htmlFor="merged-gate-operator">操作者</Label>
        <Input
          id="merged-gate-operator"
          className="w-28"
          value={operator}
          onChange={(e) => setOperator(e.target.value)}
        />
      </div>
      <Button size="sm" onClick={() => void save()} disabled={saving}>
        保存
      </Button>
      <p className="min-w-0 flex-1 text-xs">
        {armed ? (
          <span className="font-medium text-amber-600">新链将接管裁决，可随时切回 off —— 再点一次「保存」确认</span>
        ) : (
          <span className="text-muted-foreground">
            当前生效：{MODE_LABEL[current]}。建议先影子跑 48h 核对无误伤再切 enforce。
          </span>
        )}
      </p>
    </div>
  );
}

// RowDraft 是一行种类的可编辑草稿（行内编辑，保存才落账）。
interface RowDraft {
  action: SodCategoryAction;
  cap: string;
  fallback: SodCategoryFallback;
  enabled: boolean;
  priority: string;
}

// CategoryRow 是种类表的一行：动作/上限/兜底/启停/优先级行内编辑，保存走部分更新。
function CategoryRow({ row, onChanged }: { row: SodCategory; onChanged: () => Promise<void> }) {
  const [draft, setDraft] = React.useState<RowDraft>(() => ({
    action: row.action,
    cap: String(row.cap_per_task),
    fallback: row.fallback,
    enabled: row.enabled,
    priority: String(row.priority),
  }));
  const [busy, setBusy] = React.useState(false);

  const capNum = Number.parseInt(draft.cap, 10);
  const prioNum = Number.parseInt(draft.priority, 10);
  const dirty =
    draft.action !== row.action ||
    (Number.isFinite(capNum) && capNum !== row.cap_per_task) ||
    draft.fallback !== row.fallback ||
    draft.enabled !== row.enabled ||
    (Number.isFinite(prioNum) && prioNum !== row.priority);

  function patch(p: Partial<RowDraft>) {
    setDraft((d) => ({ ...d, ...p }));
  }

  async function save() {
    setBusy(true);
    try {
      const body: {
        action?: SodCategoryAction;
        cap_per_task?: number;
        fallback?: SodCategoryFallback;
        enabled?: boolean;
        priority?: number;
      } = {};
      // 只送改过的字段；锁定基线行不送 action——前端不给改，后端也会 409。
      if (!row.locked && draft.action !== row.action) body.action = draft.action;
      if (Number.isFinite(capNum) && capNum !== row.cap_per_task) body.cap_per_task = capNum;
      if (draft.fallback !== row.fallback) body.fallback = draft.fallback;
      if (draft.enabled !== row.enabled) body.enabled = draft.enabled;
      if (Number.isFinite(prioNum) && prioNum !== row.priority) body.priority = prioNum;
      await api.updateSodCategory(row.id, body);
      toast.success(`种类「${row.name}」已保存`);
      await onChanged();
    } catch (e) {
      toast.error(`保存失败：${(e as Error).message}`);
    } finally {
      setBusy(false);
    }
  }

  async function remove() {
    setBusy(true);
    try {
      await api.deleteSodCategory(row.id);
      toast.success(`种类「${row.name}」已删除`);
      await onChanged();
    } catch (e) {
      // 409（锁定基线不可删）等后端错误原文透传。
      toast.error((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <TableRow className={!row.enabled ? "opacity-50" : undefined}>
      <TableCell>
        <div className="flex items-center gap-1.5">
          <span className="whitespace-nowrap font-medium text-sm">{row.name}</span>
          {row.locked && (
            <Badge variant="outline" className="px-1 py-0 text-[10px]" title="锁定基线：动作不可改、不可删">
              <LockIcon className="size-3" /> 锁定
            </Badge>
          )}
        </div>
        {row.reason && (
          <p className="line-clamp-1 max-w-44 text-[10px] text-muted-foreground" title={row.reason}>
            {row.reason}
          </p>
        )}
      </TableCell>
      <TableCell>
        <Badge variant={row.kind === "builtin" ? "secondary" : "outline"} className="text-[10px]">
          {row.kind === "builtin" ? "内置" : "custom"}
        </Badge>
      </TableCell>
      <TableCell className="max-w-40">
        <code className="block truncate rounded bg-muted px-1.5 py-0.5 font-mono text-xs" title={row.pattern}>
          {row.pattern || "—"}
        </code>
      </TableCell>
      <TableCell>
        <Select
          value={draft.action}
          onValueChange={(v) => patch({ action: v as SodCategoryAction })}
          disabled={row.locked}
        >
          <SelectTrigger className="h-8 w-24" title={row.locked ? "锁定基线：动作不可修改" : undefined}>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {ACTION_OPTIONS.map((o) => (
              <SelectItem key={o.value} value={o.value}>
                {o.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </TableCell>
      <TableCell>
        <Input
          type="number"
          min={0}
          className="h-8 w-16"
          value={draft.cap}
          title="每任务上限，0 = 不限"
          onChange={(e) => patch({ cap: e.target.value })}
        />
      </TableCell>
      <TableCell>
        <Select value={draft.fallback} onValueChange={(v) => patch({ fallback: v as SodCategoryFallback })}>
          <SelectTrigger className="h-8 w-24" title="auditor 故障/超时/超限时的兜底">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {FALLBACK_OPTIONS.map((o) => (
              <SelectItem key={o.value} value={o.value}>
                {o.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </TableCell>
      <TableCell className="text-center">
        <Switch checked={draft.enabled} onCheckedChange={(v) => patch({ enabled: v })} />
      </TableCell>
      <TableCell>
        <Input
          type="number"
          className="h-8 w-16"
          value={draft.priority}
          title="数字越大越先匹配"
          onChange={(e) => patch({ priority: e.target.value })}
        />
      </TableCell>
      <TableCell>
        <div className="flex items-center justify-end gap-1">
          <Button
            size="sm"
            variant="outline"
            className="h-7 px-2 text-xs"
            disabled={!dirty || busy}
            onClick={() => void save()}
          >
            保存
          </Button>
          {!row.locked && (
            <Button
              size="icon"
              variant="ghost"
              className="h-7 w-7 text-destructive hover:text-destructive"
              disabled={busy}
              title="删除该种类"
              onClick={() => void remove()}
            >
              <Trash2Icon className="h-3.5 w-3.5" />
            </Button>
          )}
        </div>
      </TableCell>
    </TableRow>
  );
}

// NewForm 是「新增 custom 种类」的表单状态。
interface NewForm {
  name: string;
  pattern: string;
  action: SodCategoryAction;
  cap: number;
  fallback: SodCategoryFallback;
  priority: number;
  reason: string;
  createdBy: string;
}

const defaultNewForm = (): NewForm => ({
  name: "",
  pattern: "",
  action: "hard_deny",
  cap: 0,
  fallback: "ask",
  priority: 40,
  reason: "",
  createdBy: "sir",
});

export function SodCategoryCard() {
  const [categories, setCategories] = React.useState<SodCategory[]>([]);
  const [version, setVersion] = React.useState("");
  const [loaded, setLoaded] = React.useState(false);
  const [creating, setCreating] = React.useState(false);
  const [form, setForm] = React.useState<NewForm>(defaultNewForm);

  const load = React.useCallback(async () => {
    try {
      const r = await api.sodCategories();
      setCategories(r.categories);
      setVersion(r.version);
      setLoaded(true);
    } catch (e) {
      toast.error(`读取拦截种类失败：${(e as Error).message}`);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  function patchForm(p: Partial<NewForm>) {
    setForm((f) => ({ ...f, ...p }));
  }

  async function create() {
    if (!form.name.trim()) {
      toast.error("名称必填");
      return;
    }
    if (!form.pattern.trim()) {
      toast.error("custom 种类必须给 pattern（正则）");
      return;
    }
    if (!form.createdBy.trim()) {
      toast.error("必须留操作者身份（created_by）");
      return;
    }
    setCreating(true);
    try {
      await api.createSodCategory({
        name: form.name.trim(),
        kind: "custom",
        pattern: form.pattern.trim(),
        action: form.action,
        cap_per_task: Number.isFinite(form.cap) ? form.cap : 0,
        fallback: form.fallback,
        priority: Number.isFinite(form.priority) ? form.priority : 40,
        reason: form.reason.trim(),
        created_by: form.createdBy.trim(),
      });
      toast.success(`种类「${form.name.trim()}」已创建，立即生效`);
      setForm(defaultNewForm());
      await load();
    } catch (e) {
      toast.error((e as Error).message);
    } finally {
      setCreating(false);
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <ShieldAlertIcon className="size-4" /> 拦截种类与裁决策略
        </CardTitle>
        <CardDescription>
          你定义「该拦什么」：按优先级（数字越大越先）逐类匹配，首条命中生效；快路径就是表里 action=pass
          的行，改动作即全链生效。锁定基线行（🔒）动作不可改、不可删。
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-3">
        <MergedGateBar />
        <div className="flex items-center justify-between">
          <p className="font-medium text-sm">
            现有种类（{categories.length}）
            {version && <span className="ml-2 font-normal text-muted-foreground text-xs">表版本 {version}</span>}
          </p>
          <Button size="sm" variant="ghost" className="h-7 text-xs" onClick={() => void load()}>
            <RefreshCwIcon /> 刷新
          </Button>
        </div>
        {!loaded && <p className="py-6 text-center text-muted-foreground text-sm">加载中…</p>}
        {loaded && categories.length === 0 && (
          <p className="py-6 text-center text-muted-foreground text-sm">
            还没有拦截种类——后端启动时会自动播种默认配置（删除/破坏 🔒、情报源 GET、出网新主机等）。
          </p>
        )}
        {loaded && categories.length > 0 && (
          <div className="overflow-x-auto rounded-md border">
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>种类</TableHead>
                  <TableHead className="w-[70px]">kind</TableHead>
                  <TableHead>匹配器</TableHead>
                  <TableHead className="w-[110px]">动作</TableHead>
                  <TableHead className="w-[80px]">上限</TableHead>
                  <TableHead className="w-[110px]">兜底</TableHead>
                  <TableHead className="w-[64px] text-center">启用</TableHead>
                  <TableHead className="w-[80px]">优先级</TableHead>
                  <TableHead className="w-[110px]" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {categories.map((c) => (
                  <CategoryRow key={c.id} row={c} onChanged={load} />
                ))}
              </TableBody>
            </Table>
          </div>
        )}
        <Separator />
        <div className="grid gap-2 rounded-md border p-3">
          <p className="font-medium text-sm">新增 custom 种类</p>
          <div className="flex flex-wrap items-end gap-2">
            <div className="grid gap-1.5">
              <Label htmlFor="new-cat-name">名称</Label>
              <Input
                id="new-cat-name"
                className="w-36"
                placeholder="例：nmap 扫描"
                value={form.name}
                onChange={(e) => patchForm({ name: e.target.value })}
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="new-cat-pattern">pattern（正则）</Label>
              <Input
                id="new-cat-pattern"
                className="w-56 font-mono"
                placeholder="\bnmap\b"
                value={form.pattern}
                onChange={(e) => patchForm({ pattern: e.target.value })}
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="new-cat-action">动作</Label>
              <Select value={form.action} onValueChange={(v) => patchForm({ action: v as SodCategoryAction })}>
                <SelectTrigger id="new-cat-action" className="w-44">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="hard_deny">硬拒 — 直接拒绝</SelectItem>
                  <SelectItem value="ask">转人工 — 等审批</SelectItem>
                  <SelectItem value="auditor">审计裁决 — 交 auditor</SelectItem>
                  <SelectItem value="pass">放行 — 零 LLM 快路径</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="new-cat-cap">每任务上限</Label>
              <Input
                id="new-cat-cap"
                type="number"
                min={0}
                className="w-20"
                value={form.cap}
                title="0 = 不限"
                onChange={(e) => patchForm({ cap: Number.parseInt(e.target.value, 10) || 0 })}
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="new-cat-fallback">兜底</Label>
              <Select value={form.fallback} onValueChange={(v) => patchForm({ fallback: v as SodCategoryFallback })}>
                <SelectTrigger id="new-cat-fallback" className="w-28">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {FALLBACK_OPTIONS.map((o) => (
                    <SelectItem key={o.value} value={o.value}>
                      {o.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="new-cat-priority">优先级</Label>
              <Input
                id="new-cat-priority"
                type="number"
                className="w-20"
                value={form.priority}
                onChange={(e) => patchForm({ priority: Number.parseInt(e.target.value, 10) || 0 })}
              />
            </div>
          </div>
          <div className="flex flex-wrap items-end gap-2">
            <div className="grid min-w-52 flex-1 gap-1.5">
              <Label htmlFor="new-cat-reason">理由（审计留痕）</Label>
              <Input
                id="new-cat-reason"
                placeholder="为什么要拦/放这一类"
                value={form.reason}
                onChange={(e) => patchForm({ reason: e.target.value })}
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="new-cat-createdby">操作者</Label>
              <Input
                id="new-cat-createdby"
                className="w-28"
                value={form.createdBy}
                onChange={(e) => patchForm({ createdBy: e.target.value })}
              />
            </div>
            <Button size="sm" disabled={creating} onClick={() => void create()}>
              <PlusIcon /> 保存新种类
            </Button>
          </div>
          <p className="text-muted-foreground text-xs">
            例：pattern=<code className="font-mono">\bnmap\b</code>
            、动作=硬拒，保存后立即生效、全审计。含锁定基线特征的正则不可配成放行（后端 409）。
          </p>
        </div>
      </CardContent>
    </Card>
  );
}
