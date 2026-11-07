"use client";

// 「发布任务」表单（POST /api/task-hall/tasks）：
// 一次调用把 目标+成员(+额度) 变成一个能干活的大厅 —— 后端 PublishTask 依次
// 建任务 → 声明资产范围 → 建群（大厅会话 + 成员会话）→ 可选开额度。
// 与 CreateGroupDialog（给已有任务补建大厅）互为补充：一个从零发布、一个挂已有任务。

import * as React from "react";

import { RocketIcon, WandSparklesIcon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Spinner } from "@/components/ui/spinner";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type { Agent, Asset, TaskCategory } from "@/lib/types";

export function PublishTaskDialog({ onPublished }: { onPublished: (taskID: number, groupID: number) => void }) {
  const [open, setOpen] = React.useState(false);
  const [agents, setAgents] = React.useState<Agent[]>([]);
  const [assets, setAssets] = React.useState<Asset[]>([]);
  const [assetPicked, setAssetPicked] = React.useState<number[]>([]);
  const [goal, setGoal] = React.useState("");
  const [name, setName] = React.useState("");
  const [description, setDescription] = React.useState("");
  const [picked, setPicked] = React.useState<string[]>([]);
  const [categories, setCategories] = React.useState<TaskCategory[]>([]);
  // 类别下拉的值："" = 未分类（NativeSelect 的空串选项，与任务模板同一交互）。
  const [categoryId, setCategoryId] = React.useState("");
  const [concurrency, setConcurrency] = React.useState("3");
  const [budget, setBudget] = React.useState("");
  const [timeoutSec, setTimeoutSec] = React.useState("7200");
  const [saving, setSaving] = React.useState(false);

  React.useEffect(() => {
    if (!open) return;
    api
      .agents()
      .then(setAgents)
      .catch(() => setAgents([]));
    // 资产范围是发布链路的要害（成员只认 task_scope 声明的资产）⇒ 打开即拉一页供勾选。
    api
      .assets("", 200, 0)
      .then((r) => setAssets(r.assets))
      .catch(() => setAssets([]));
    // 类别选项来自 task_categories（内置三类由后端首次启动播种）。拉不到就不阻塞
    // 发布——类别是纪律开关不是必填项，下拉退化为「未分类」。
    api
      .taskCategories()
      .then(setCategories)
      .catch(() => setCategories([]));
  }, [open]);

  // auto 恒为分发者（后端发布流程自动写入），不在可选成员里。
  const candidates = agents.filter((a) => a.key !== "auto");

  function toggle(key: string) {
    setPicked((prev) => (prev.includes(key) ? prev.filter((k) => k !== key) : [...prev, key]));
  }

  function toggleAsset(id: number) {
    setAssetPicked((prev) => (prev.includes(id) ? prev.filter((v) => v !== id) : [...prev, id]));
  }

  // applyReconTemplate 一键填「资产收集」快捷模板（方案 v4 §3-C3）：goal 模板 +
  // 类别=资产收集。X 是占位——模板没法替用户猜目标，弹 toast 提醒替换。
  function applyReconTemplate() {
    setGoal(
      "对目标 X 进行资产收集：覆盖根域名/子域名、IP 网段、应用指纹、对外服务与接口；" +
        "先被动源（证书透明/测绘引擎/历史档案）后主动枚举；一切发现登记入库并标注来源与置信度；" +
        "范围外资产只记录不探测；禁止利用类动作。",
    );
    const recon = categories.find((c) => c.name.trim().toLowerCase() === "资产收集");
    if (recon) {
      setCategoryId(String(recon.id));
    } else {
      toast.warning("类别列表里还没有「资产收集」——请确认后端已升级（首次启动自动播种三个内置类别）");
    }
    toast.info("已填入资产收集模板：把 goal 里的 X 替换成实际目标");
  }

  function reset() {
    setGoal("");
    setName("");
    setDescription("");
    setPicked([]);
    setAssetPicked([]);
    setCategoryId("");
    setConcurrency("3");
    setBudget("");
    setTimeoutSec("7200");
  }

  async function submit() {
    const trimmedGoal = goal.trim();
    if (saving) return;
    if (trimmedGoal === "") {
      toast.error("任务目标必填：说明要做什么、达成什么");
      return;
    }
    if (assetPicked.length === 0) {
      toast.error("至少勾选一个资产：成员只认任务范围内声明的资产");
      return;
    }
    const budgetNum = Number(budget.trim());
    if (!Number.isFinite(budgetNum) || budgetNum <= 0) {
      toast.error("额度（tokens）必填：大厅群组的模型请求走 fail-closed 额度闸");
      return;
    }
    const limit = Number(concurrency.trim());
    const _budgetTokens = Number(budget.trim());
    // 任务墙钟（C3）：空 = 后端默认 7200；0 = 显式不限时；负数/非数字 = 参数错误。
    let timeoutSeconds: number | undefined;
    if (timeoutSec.trim() !== "") {
      const t = Number(timeoutSec.trim());
      if (!Number.isFinite(t) || t < 0) {
        toast.error("任务时限必须是非负整数秒（0 = 不限时）");
        return;
      }
      timeoutSeconds = Math.floor(t);
    }
    setSaving(true);
    try {
      const r = await api.publishTaskHallTask({
        goal: trimmedGoal,
        asset_ids: assetPicked,
        ...(name.trim() ? { name: name.trim() } : {}),
        ...(description.trim() ? { description: description.trim() } : {}),
        ...(picked.length > 0 ? { members: picked } : {}),
        ...(Number.isFinite(limit) && limit > 0 ? { concurrency_limit: limit } : {}),
        budget_tokens: budgetNum,
        ...(categoryId !== "" ? { category_id: Number(categoryId) } : {}),
        ...(timeoutSeconds !== undefined ? { timeout_seconds: timeoutSeconds } : {}),
      });
      toast.success(`任务 #${r.task_id} 已发布：大厅 #${r.group_id} 已建好，auto 开始派工`);
      setOpen(false);
      reset();
      onPublished(r.task_id, r.group_id);
    } catch (e) {
      toast.error(`发布失败：${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        setOpen(next);
        if (!next) reset();
      }}
    >
      <DialogTrigger asChild>
        <Button size="sm" variant="outline">
          <RocketIcon /> 发布任务
        </Button>
      </DialogTrigger>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>发布任务到大厅</DialogTitle>
          <DialogDescription>
            填一个目标即可：自动建任务并开大厅，auto 派工，成员各拿独立会话共享同一张探索图。
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-4 py-2">
          <div className="flex items-center justify-between gap-2">
            <Label htmlFor="pub-goal">任务目标 *</Label>
            <Button
              type="button"
              size="sm"
              variant="secondary"
              className="h-7 px-2 text-xs"
              onClick={applyReconTemplate}
              title="一键填入资产收集 goal 模板并把类别设为「资产收集」"
            >
              <WandSparklesIcon /> 资产收集模板
            </Button>
          </div>
          <div className="grid gap-1.5">
            <Textarea
              id="pub-goal"
              rows={4}
              placeholder="例：对 192.168.132.77 做端口与服务探测，只针对这个 IP；或点上方「资产收集模板」一键填入"
              value={goal}
              onChange={(e) => setGoal(e.target.value)}
            />
          </div>

          <div className="grid grid-cols-2 gap-3">
            <div className="grid gap-1.5">
              <Label htmlFor="pub-name">任务名称</Label>
              <Input
                id="pub-name"
                placeholder="留空则用目标首行"
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="pub-category">任务类别</Label>
              <NativeSelect
                id="pub-category"
                className="w-full"
                value={categoryId}
                onChange={(e) => setCategoryId(e.target.value)}
              >
                <NativeSelectOption value="">未分类</NativeSelectOption>
                {categories.map((c) => (
                  <NativeSelectOption key={c.id} value={String(c.id)}>
                    {c.name}
                  </NativeSelectOption>
                ))}
              </NativeSelect>
            </div>
          </div>
          <p className="-mt-2 text-muted-foreground text-xs">
            类别=资产收集时，引擎会给任务 worker 追加「只收集不利用、发现即入库」的收集纪律。
          </p>

          <div className="grid gap-1.5">
            <Label htmlFor="pub-desc">补充说明</Label>
            <Textarea
              id="pub-desc"
              rows={2}
              placeholder="可选：范围约束、注意事项、验收标准"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
            />
          </div>

          <div className="grid gap-1.5">
            <Label>资产范围 *（成员只认范围内声明的资产）</Label>
            <div className="rounded-lg border">
              <ScrollArea className="h-32">
                <div className="grid gap-0.5 p-1">
                  {assets.length === 0 && (
                    <p className="px-2 py-3 text-center text-muted-foreground text-xs">
                      还没有资产 —— 先到「资产」页添加，或稍后用 add_company_scope 追加
                    </p>
                  )}
                  {assets.map((a) => (
                    <label
                      key={a.id}
                      htmlFor={`pub-asset-${a.id}`}
                      className="flex cursor-pointer items-center gap-2 rounded-md px-2 py-1.5 hover:bg-accent"
                    >
                      <Checkbox
                        id={`pub-asset-${a.id}`}
                        checked={assetPicked.includes(a.id)}
                        onCheckedChange={() => toggleAsset(a.id)}
                      />
                      <Badge variant="outline" className="px-1.5 py-0 text-[10px] uppercase">
                        {a.type}
                      </Badge>
                      <span className="min-w-0 flex-1 truncate font-mono text-xs">
                        {a.domain || a.root_domain || a.ip || a.icp || a.c_segment || `#${a.id}`}
                      </span>
                    </label>
                  ))}
                </div>
              </ScrollArea>
              <p className="px-3 py-1.5 text-muted-foreground text-xs">已选 {assetPicked.length} 个</p>
            </div>
          </div>

          <div className="grid gap-1.5">
            <Label>成员</Label>
            <div className="rounded-lg border">
              <div className="flex items-center gap-2 rounded-t-lg border-b bg-muted/50 px-3 py-2">
                <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
                  auto · 分发者
                </Badge>
                <span className="text-muted-foreground text-xs">auto 自动入席并负责派工/兜底。</span>
              </div>
              <ScrollArea className="h-32">
                <div className="grid gap-0.5 p-1">
                  {candidates.length === 0 && (
                    <p className="px-2 py-3 text-center text-muted-foreground text-xs">没有可加入的 Agent</p>
                  )}
                  {candidates.map((a) => (
                    <label
                      key={a.key}
                      htmlFor={`pub-member-${a.key}`}
                      className="flex cursor-pointer items-center gap-2 rounded-md px-2 py-1.5 hover:bg-accent"
                    >
                      <Checkbox
                        id={`pub-member-${a.key}`}
                        checked={picked.includes(a.key)}
                        onCheckedChange={() => toggle(a.key)}
                      />
                      <span className="text-sm">{a.name}</span>
                      <span className="font-mono text-muted-foreground text-xs">{a.key}</span>
                    </label>
                  ))}
                </div>
              </ScrollArea>
              <p className="px-3 py-1.5 text-muted-foreground text-xs">
                留空 = 只建大厅稍后热加入；成员从任务资产范围里领活。
              </p>
            </div>
          </div>

          <div className="grid grid-cols-2 gap-3">
            <div className="grid gap-1.5">
              <Label htmlFor="pub-limit">组内并发上限</Label>
              <Input
                id="pub-limit"
                inputMode="numeric"
                value={concurrency}
                onChange={(e) => setConcurrency(e.target.value)}
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="pub-budget">额度（tokens，必填）</Label>
              <Input
                id="pub-budget"
                inputMode="numeric"
                placeholder="例：200000"
                value={budget}
                onChange={(e) => setBudget(e.target.value)}
              />
            </div>
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="pub-timeout">任务时限（秒，到点有序收尾；0 = 不限时）</Label>
            <Input
              id="pub-timeout"
              inputMode="numeric"
              placeholder="默认 7200（2 小时）"
              value={timeoutSec}
              onChange={(e) => setTimeoutSec(e.target.value)}
            />
          </div>
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => setOpen(false)}>
            取消
          </Button>
          <Button onClick={() => void submit()} disabled={saving}>
            {saving && <Spinner className="size-4" />}
            发布
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
