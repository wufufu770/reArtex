"use client";

import * as React from "react";

import { PlusIcon } from "lucide-react";
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
import { ScrollArea } from "@/components/ui/scroll-area";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { api } from "@/lib/api";
import type { Agent, Task } from "@/lib/types";

const NO_TASK = "__none__";

// CreateGroupDialog 是「新建任务大厅」的表单。
//
// 建群走 POST /api/task-hall/groups（task_id / name / members / concurrency_limit），
// 额度另走一步 PUT /api/task-hall/groups/{id}/quota —— 建群接口不收额度字段，
// 而额度接口要求带变更理由（理由必填，事后要追责）。
export function CreateGroupDialog({ onCreated }: { onCreated: (groupId: number) => void }) {
  const [open, setOpen] = React.useState(false);
  const [tasks, setTasks] = React.useState<Task[]>([]);
  const [agents, setAgents] = React.useState<Agent[]>([]);
  const [taskId, setTaskId] = React.useState<string>(NO_TASK);
  const [name, setName] = React.useState("");
  const [picked, setPicked] = React.useState<string[]>([]);
  const [concurrency, setConcurrency] = React.useState("");
  const [budget, setBudget] = React.useState("");
  const [saving, setSaving] = React.useState(false);

  const load = React.useCallback(() => {
    api
      .tasks()
      .then((r) => setTasks(r.tasks))
      .catch(() => setTasks([]));
    api
      .agents()
      .then(setAgents)
      .catch(() => setAgents([]));
  }, []);

  React.useEffect(() => {
    if (!open) return;
    load();
  }, [open, load]);

  // auto 恒为分发者（后端建群时自动写入），所以它不在可选成员里。
  const candidates = agents.filter((a) => a.key !== "auto");
  const selected = picked.length > 0 ? picked : [];

  function toggle(key: string) {
    setPicked((prev) => (prev.includes(key) ? prev.filter((k) => k !== key) : [...prev, key]));
  }

  function reset() {
    setTaskId(NO_TASK);
    setName("");
    setPicked([]);
    setConcurrency("");
    setBudget("");
  }

  const limit = Number(concurrency.trim());
  const budgetTokens = Number(budget.trim());
  const canSubmit = taskId !== NO_TASK && selected.length > 0 && !saving;

  async function submit() {
    if (taskId === NO_TASK) {
      toast.error("请先选择任务");
      return;
    }
    if (selected.length === 0) {
      toast.error("至少选择一个成员");
      return;
    }
    setSaving(true);
    try {
      const group = await api.createTaskHallGroup({
        task_id: Number(taskId),
        name: name.trim(),
        members: selected,
        concurrency_limit: Number.isFinite(limit) && limit > 0 ? limit : selected.length,
      });
      // 额度是第二步：建群成功后单独调额度接口（理由必填）。
      if (Number.isFinite(budgetTokens) && budgetTokens > 0) {
        try {
          await api.setTaskHallQuota(group.id, {
            budget_tokens: budgetTokens,
            reason: "建群时设定额度",
          });
        } catch (e) {
          toast.error(`大厅已创建，但额度设置失败：${(e as Error).message}`);
          setOpen(false);
          reset();
          onCreated(group.id);
          return;
        }
      }
      toast.success("已创建任务大厅");
      setOpen(false);
      reset();
      onCreated(group.id);
    } catch (e) {
      toast.error(`创建失败：${(e as Error).message}`);
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
        <Button size="sm">
          <PlusIcon /> 新建大厅
        </Button>
      </DialogTrigger>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>新建任务大厅</DialogTitle>
          <DialogDescription>
            一个任务至多一个大厅。大厅共享该任务的探索图与用量，成员各拿一个独立会话。
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-4 py-2">
          <div className="grid gap-1.5">
            <Label htmlFor="hall-task">任务</Label>
            <Select value={taskId} onValueChange={setTaskId}>
              <SelectTrigger id="hall-task" className="w-full">
                <SelectValue placeholder="选择要建大厅的任务" />
              </SelectTrigger>
              <SelectContent>
                {tasks.length === 0 && <SelectItem value={NO_TASK}>没有可用任务</SelectItem>}
                {tasks.map((t) => (
                  <SelectItem key={t.id} value={t.id}>
                    #{t.id} · {t.name || t.description || "未命名任务"}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {tasks.length === 0 && (
              <p className="text-muted-foreground text-xs">还没有任务 —— 先到「任务」页建一个任务再来建大厅。</p>
            )}
          </div>

          <div className="grid gap-1.5">
            <Label htmlFor="hall-name">大厅名称</Label>
            <Input id="hall-name" placeholder="留空则用任务名" value={name} onChange={(e) => setName(e.target.value)} />
          </div>

          <div className="grid gap-1.5">
            <Label>成员</Label>
            <div className="rounded-lg border">
              <div className="flex items-center gap-2 rounded-t-lg border-b bg-muted/50 px-3 py-2">
                <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
                  auto · 分发者
                </Badge>
                <span className="text-muted-foreground text-xs">
                  auto 是分发者：群里没有可用成员时它兜底执行；分发时它不与成员抢单。
                </span>
              </div>
              <ScrollArea className="h-40">
                <div className="grid gap-0.5 p-1">
                  {candidates.length === 0 && (
                    <p className="px-2 py-3 text-center text-muted-foreground text-xs">没有可加入的 Agent</p>
                  )}
                  {candidates.map((a) => (
                    <label
                      key={a.key}
                      htmlFor={`hall-member-${a.key}`}
                      className="flex cursor-pointer items-center gap-2 rounded-md px-2 py-1.5 hover:bg-accent"
                    >
                      <Checkbox
                        id={`hall-member-${a.key}`}
                        checked={selected.includes(a.key)}
                        onCheckedChange={() => toggle(a.key)}
                      />
                      <span className="text-sm">{a.name}</span>
                      <span className="font-mono text-muted-foreground text-xs">{a.key}</span>
                      {!a.enabled && (
                        <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
                          已停用
                        </Badge>
                      )}
                    </label>
                  ))}
                </div>
              </ScrollArea>
            </div>
            <p className="text-muted-foreground text-xs">已选 {selected.length} 位成员（至少 1 位）。</p>
          </div>

          <div className="grid gap-4 sm:grid-cols-2">
            <div className="grid gap-1.5">
              <Label htmlFor="hall-concurrency">并发上限</Label>
              <Input
                id="hall-concurrency"
                type="number"
                min={1}
                placeholder={String(Math.max(selected.length, 1))}
                value={concurrency}
                onChange={(e) => setConcurrency(e.target.value)}
              />
              <p className="text-muted-foreground text-xs">留空用成员数。群内在跑的意图数达到上限后不再接新单。</p>
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="hall-budget">token 额度</Label>
              <Input
                id="hall-budget"
                type="number"
                min={1}
                placeholder="不限（不配置额度）"
                value={budget}
                onChange={(e) => setBudget(e.target.value)}
              />
              <p className="text-muted-foreground text-xs">留空表示不配置额度 —— 没有额度行的群不允许发起模型调用。</p>
            </div>
          </div>
        </div>

        <DialogFooter>
          <Button onClick={() => void submit()} disabled={!canSubmit}>
            {saving && <Spinner data-icon="inline-start" />}
            {saving ? "创建中" : "创建大厅"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
