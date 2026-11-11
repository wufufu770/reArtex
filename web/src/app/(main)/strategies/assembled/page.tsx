"use client";

// 注入拼装页（v0.9.17）：agent 实际拿到的整份注入文本。
//
// 用户原话：「策略注入进去，我可以直接看拼好的整个库，然后这个库会被 agent 调用」。
// 两块文本与生产注入路径**同源渲染**（后端同一份函数）：
//   - 策略摘要块：planner 每轮规划拿到的（快照按任务同版本）；
//   - 经验块：新任务种子路径拿到的（accepted 池 top-5、≤12K 预算）。
// 本页是只读预览：后端对经验块走不记账变体，刷新不会污染命中计数。

import * as React from "react";

import { FileTextIcon, HistoryIcon, LibraryBigIcon } from "lucide-react";
import { toast } from "sonner";

import { CopyButton } from "@/components/copy-button";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { api } from "@/lib/api";
import type { Task } from "@/lib/types";

import { StrategyNav } from "../_components/strategy-nav";

// 长文本块的展示上限：注入块本身就带预算（12K），全量渲染没有意义；
// 再叠一层防止极端情况下页面被拖爆。
const BLOCK_MAX = 40_000;

function clampBlock(s: string): string {
  if (s.length <= BLOCK_MAX) return s;
  return s.slice(0, BLOCK_MAX) + `\n…（截断展示：原文 ${s.length} 字符）`;
}

function InjectionBlock({
  title,
  icon,
  text,
  description,
}: {
  title: string;
  icon: React.ReactNode;
  text: string;
  description: string;
}) {
  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between gap-2">
        <div className="grid gap-1">
          <CardTitle className="flex items-center gap-2 text-base">
            {icon}
            {title}
          </CardTitle>
          <CardDescription>{description}</CardDescription>
        </div>
        <CopyButton text={text} />
      </CardHeader>
      <CardContent>
        <pre className="max-h-[28rem] overflow-y-auto rounded-md border bg-muted/40 p-3 font-mono text-xs whitespace-pre-wrap">
          {text}
        </pre>
      </CardContent>
    </Card>
  );
}

export default function StrategyAssembledPage() {
  const [tasks, setTasks] = React.useState<Task[]>([]);
  const [taskId, setTaskId] = React.useState<string>("__all__");
  const [data, setData] = React.useState<{
    strategies_block: string;
    legacy_block: string;
    entries: { strategy_id: number; name: string; summary: string; version: number; utility: number }[];
    note: string;
  } | null>(null);
  const [loaded, setLoaded] = React.useState(false);

  const load = React.useCallback(async () => {
    try {
      const numeric = taskId === "__all__" ? undefined : Number(taskId);
      const goal = taskId === "__all__" ? undefined : tasks.find((t) => t.id === taskId)?.goal;
      const r = await api.strategyAssembled({ taskId: numeric, goal });
      setData(r);
    } catch (e) {
      toast.error(`读取注入拼装失败：${(e as Error).message}`);
    } finally {
      setLoaded(true);
    }
  }, [taskId, tasks]);

  React.useEffect(() => {
    api
      .tasks()
      .then((r) => setTasks(r.tasks))
      .catch(() => setTasks([]));
  }, []);

  React.useEffect(() => {
    setLoaded(false);
    void load();
  }, [load]);

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="font-semibold text-xl tracking-tight">注入拼装</h1>
          <p className="text-muted-foreground text-sm">
            agent 实际拿到的整份注入文本：策略摘要块（planner 规划用）+ 经验块（新任务种子用）。与生产注入路径同源渲染。
          </p>
        </div>
        <Select
          value={taskId}
          onValueChange={(v) => {
            setTaskId(v);
          }}
        >
          <SelectTrigger className="w-full sm:w-72">
            <SelectValue placeholder="选择视角" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="__all__">全库视角（active 池全量摘要）</SelectItem>
            {tasks.slice(0, 30).map((t) => (
              <SelectItem key={t.id} value={t.id}>
                任务 #{t.id} {(t.name || t.description || "").slice(0, 30)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>

      <StrategyNav current="assembled" />

      {data && (
        <p className="flex flex-wrap items-center gap-2 text-muted-foreground text-xs">
          <Badge variant="outline" className="gap-1 px-1.5 py-0 text-[10px]">
            <LibraryBigIcon className="size-3" />
            策略 {data.entries.length} 条
          </Badge>
          <span>{data.note}</span>
        </p>
      )}

      {!loaded && <p className="py-8 text-center text-muted-foreground text-sm">加载中…</p>}
      {loaded && data && (
        <div className="grid gap-4">
          <InjectionBlock
            title="策略摘要块（planner 注入）"
            icon={<LibraryBigIcon className="size-4" />}
            text={clampBlock(data.strategies_block)}
            description="planner 每轮规划拿到的策略摘要（只带 name+summary+utility，绝不带 steps 正文）。任务视角 = 该任务的真实快照（组内同版本）。"
          />
          <InjectionBlock
            title="经验块（新任务种子注入）"
            icon={<HistoryIcon className="size-4" />}
            text={clampBlock(data.legacy_block)}
            description="新任务启动时按目标检索 accepted 经验池渲染的注入块（top-5、≤12K 字符预算、加权重排）。任务视角会以该任务目标文本做同族检索。"
          />
          {data.entries.length > 0 && (
            <Card>
              <CardHeader>
                <CardTitle className="flex items-center gap-2 text-base">
                  <FileTextIcon className="size-4" />
                  本次命中的策略条目
                </CardTitle>
                <CardDescription>
                  {taskId === "__all__" ? "active 池全量" : "该任务快照选中的条目（检索按资产签名匹配）"}
                </CardDescription>
              </CardHeader>
              <CardContent className="grid max-h-96 gap-2 overflow-y-auto">
                {data.entries.map((e) => (
                  <div key={e.strategy_id} className="grid gap-0.5 rounded-md border p-2">
                    <div className="flex items-center gap-2">
                      <span className="font-mono text-[10px] text-muted-foreground">
                        #{e.strategy_id} v{e.version}
                      </span>
                      <span className="min-w-0 truncate font-medium text-sm">{e.name}</span>
                      <span className="ml-auto shrink-0 font-mono text-[10px] text-muted-foreground">
                        utility={e.utility.toFixed(2)}
                      </span>
                    </div>
                    <span className="line-clamp-2 text-muted-foreground text-xs">{e.summary}</span>
                  </div>
                ))}
              </CardContent>
            </Card>
          )}
          <p className="text-muted-foreground text-xs">
            <Spinner className="mr-1 inline size-3 align-text-bottom" />
            本页为只读预览：后端对经验块走不记账变体，反复刷新不会污染命中计数与采纳留痕。
          </p>
        </div>
      )}
    </div>
  );
}
