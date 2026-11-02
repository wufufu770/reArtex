"use client";

import * as React from "react";

import Link from "next/link";
import { useSearchParams } from "next/navigation";

import { ArrowLeftIcon } from "lucide-react";

import { StatusBadge } from "@/components/status-badge";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Separator } from "@/components/ui/separator";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api } from "@/lib/api";
import type {
  Agent,
  Conversation,
  Settings,
  Task,
  TaskHallGroupDetail,
  TaskHallMessage,
  TaskHallQuota,
  TaskNode,
} from "@/lib/types";

import { IntentsTab } from "./_tabs/intents-tab";
import { MailboxTab } from "./_tabs/mailbox-tab";
import { MembersTab } from "./_tabs/members-tab";
import { OverviewTab } from "./_tabs/overview-tab";
import { QuotaTab } from "./_tabs/quota-tab";
import { ToolsTab } from "./_tabs/tools-tab";

// 仲裁面（成员状态 / Intent / 额度 / 并发）变化快，5 秒一轮；
// 全局面（任务、会话、设置）变化慢，15 秒一轮。
const FAST_MS = 5000;
const SLOW_MS = 15000;

const TABS = [
  { value: "overview", label: "总览" },
  { value: "members", label: "成员" },
  { value: "intents", label: "Intent 看板" },
  { value: "quota", label: "额度" },
  { value: "mailbox", label: "消息箱" },
  { value: "tools", label: "工具禁列表" },
];

function TaskHallDetailInner() {
  const searchParams = useSearchParams();
  const rawID = searchParams.get("id") ?? "";
  const id = Number(rawID);
  const [tab, setTab] = React.useState("overview");

  const [detail, setDetail] = React.useState<TaskHallGroupDetail | null>(null);
  const [quota, setQuota] = React.useState<TaskHallQuota | null>(null);
  const [intents, setIntents] = React.useState<TaskNode[]>([]);
  const [messages, setMessages] = React.useState<TaskHallMessage[]>([]);
  const [conversations, setConversations] = React.useState<Conversation[]>([]);
  const [tasks, setTasks] = React.useState<Task[]>([]);
  const [agents, setAgents] = React.useState<Agent[]>([]);
  const [settings, setSettings] = React.useState<Settings | null>(null);
  const [loaded, setLoaded] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  const aliveRef = React.useRef(true);
  React.useEffect(() => {
    aliveRef.current = true;
    return () => {
      aliveRef.current = false;
    };
  }, []);

  const loadFast = React.useCallback(async (groupId: number) => {
    try {
      const d = await api.taskHallGroup(groupId);
      const [q, msgs, ints] = await Promise.all([
        api.taskHallQuota(groupId),
        api.taskHallMessages(groupId),
        api.intents(String(d.task_id)).catch(() => [] as TaskNode[]),
      ]);
      if (!aliveRef.current) return;
      setDetail(d);
      setQuota(q);
      setMessages(msgs);
      setIntents(ints);
      setError(null);
    } catch (e) {
      if (!aliveRef.current) return;
      setError((e as Error).message);
    } finally {
      if (aliveRef.current) setLoaded(true);
    }
  }, []);

  const loadSlow = React.useCallback(async () => {
    try {
      const [convs, taskResp, agentList, cfg] = await Promise.all([
        api.conversations(),
        api.tasks(),
        api.agents(),
        api.settings(),
      ]);
      if (!aliveRef.current) return;
      setConversations(convs);
      setTasks(taskResp.tasks);
      setAgents(agentList);
      setSettings(cfg);
    } catch {
      // 全局面读不到时保留上一轮快照：这些数据只做参考展示。
    }
  }, []);

  React.useEffect(() => {
    if (!Number.isFinite(id)) return;
    void loadFast(id);
    void loadSlow();
    const fast = setInterval(() => void loadFast(id), FAST_MS);
    const slow = setInterval(() => void loadSlow(), SLOW_MS);
    return () => {
      clearInterval(fast);
      clearInterval(slow);
    };
  }, [id, loadFast, loadSlow]);

  const reload = React.useCallback(() => {
    if (Number.isFinite(id)) void loadFast(id);
  }, [id, loadFast]);

  if (!Number.isFinite(id)) {
    return (
      <div className="flex flex-1 flex-col items-center justify-center gap-3 p-10 text-center">
        <p className="text-muted-foreground">缺少大厅 id</p>
        <Button asChild variant="outline">
          <Link href="/task-hall">
            <ArrowLeftIcon /> 返回任务大厅
          </Link>
        </Button>
      </div>
    );
  }

  if (!detail) {
    return (
      <div className="flex flex-1 flex-col items-center justify-center gap-3 p-10 text-center">
        <p className="text-muted-foreground">{loaded ? (error ?? "大厅不存在或已被删除") : "加载中…"}</p>
        {loaded && (
          <Button asChild variant="outline">
            <Link href="/task-hall">
              <ArrowLeftIcon /> 返回任务大厅
            </Link>
          </Button>
        )}
      </div>
    );
  }

  const task = tasks.find((t) => t.id === String(detail.task_id));

  return (
    <Tabs value={tab} onValueChange={setTab} className="flex flex-1 flex-col gap-4">
      <div className="flex flex-col gap-2">
        <div className="flex items-center gap-2">
          <Button asChild variant="ghost" size="icon" className="size-7">
            <Link href="/task-hall">
              <ArrowLeftIcon />
            </Link>
          </Button>
          <h1 className="min-w-0 flex-1 truncate font-semibold text-xl tracking-tight">{detail.name}</h1>
          <code className="hidden rounded bg-muted px-1.5 py-0.5 font-mono text-muted-foreground text-xs sm:inline">
            #{detail.id}
          </code>
          <StatusBadge domain="group" value={detail.status} dot />
        </div>
        <div className="flex flex-wrap items-center gap-2 pl-9 text-muted-foreground text-xs">
          <span>任务</span>
          {task ? (
            <Link href={`/function/tasks/detail?id=${task.id}`} className="text-foreground hover:text-primary">
              {task.name || task.description || `任务 ${task.id}`}
            </Link>
          ) : (
            <span>任务 {detail.task_id}</span>
          )}
          <Separator orientation="vertical" className="h-3" />
          <span>大厅会话 #{detail.conversation_id}</span>
          <Separator orientation="vertical" className="h-3" />
          <span>消息 {detail.message_count}</span>
          <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
            并发上限 {detail.concurrency_limit}
          </Badge>
        </div>
      </div>

      <TabsList variant="default" className="min-w-max">
        {TABS.map((t) => (
          <TabsTrigger key={t.value} value={t.value}>
            {t.label}
          </TabsTrigger>
        ))}
      </TabsList>

      <TabsContent value="overview" className="mt-0">
        <OverviewTab detail={detail} quota={quota} intents={intents} task={task} tasks={tasks} settings={settings} />
      </TabsContent>
      <TabsContent value="members" className="mt-0">
        <MembersTab detail={detail} conversations={conversations} agents={agents} />
      </TabsContent>
      <TabsContent value="intents" className="mt-0">
        <IntentsTab intents={intents} />
      </TabsContent>
      <TabsContent value="quota" className="mt-0">
        <QuotaTab groupId={detail.id} quota={quota} onChanged={reload} />
      </TabsContent>
      <TabsContent value="mailbox" className="mt-0">
        <MailboxTab groupId={detail.id} detail={detail} messages={messages} intents={intents} onSent={reload} />
      </TabsContent>
      <TabsContent value="tools" className="mt-0">
        <ToolsTab groupId={detail.id} detail={detail} onChanged={reload} />
      </TabsContent>
    </Tabs>
  );
}

// useSearchParams must sit under a Suspense boundary for static export.
export default function TaskHallDetailPage() {
  return (
    <React.Suspense fallback={null}>
      <TaskHallDetailInner />
    </React.Suspense>
  );
}
