"use client";

// 三种会话形态的创建入口（旧会话 / 群聊 / 总控）。
//
// 形态与后端对齐：
//   - 群聊   → POST /api/sessions { kind: "chat" }    @ 定向必醒，回复进会话流
//   - 总控   → POST /api/sessions { kind: "master", goal? } 群聊全部能力 + 创建即编排
//             （goal 非空 ⇒ 后端建任务并绑定，进展直接播报进会话流）
//   - 旧会话 → 不在这里建：滚回 DraftChat（选 agent、首条消息懒创建）——与迁移期
//     的旧对话流同一条路，避免出现「空对话」半截状态。
//
// 群聊/总控的成员从「可对话 agent」里多选（默认 auto 为分发者）；总控另带目标框。

import * as React from "react";

import { Loader2Icon, MessagesSquare, Radar, UserRound } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type { Session } from "@/lib/session-types";
import type { Agent } from "@/lib/types";

type FormKind = "chat" | "master" | "legacy";

const FORM_META: Record<FormKind, { title: string; desc: string; icon: React.ReactNode }> = {
  chat: {
    title: "群聊",
    desc: "人 + 多个 agent 同一条消息流，@谁谁响应，回复直接进群。",
    icon: <MessagesSquare className="size-5" />,
  },
  master: {
    title: "总控",
    desc: "指挥室：创建即按目标起任务，规划/认领/执行进展自动播报进群。",
    icon: <Radar className="size-5" />,
  },
  legacy: {
    title: "旧会话",
    desc: "与单个 agent 的一对一对话（原 ARTEX 形态）。",
    icon: <UserRound className="size-5" />,
  },
};

export function CreateSessionDialog({
  open,
  onOpenChange,
  agents,
  onCreated,
  onPickLegacy,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  agents: Agent[];
  onCreated: (session: Session) => void;
  onPickLegacy: (agentKey: string) => void;
}) {
  const [form, setForm] = React.useState<FormKind>("chat");
  const [selected, setSelected] = React.useState<string[]>(["auto"]);
  const [goal, setGoal] = React.useState("");
  const [sessionName, setSessionName] = React.useState("");
  const [legacyAgent, setLegacyAgent] = React.useState("auto");
  const [submitting, setSubmitting] = React.useState(false);

  // 打开时回到默认形态（群聊），成员保留上次选择 —— 重开一次不必重新勾。
  React.useEffect(() => {
    if (open) {
      setForm("chat");
      setGoal("");
      setSessionName("");
    }
  }, [open]);

  const toggleAgent = (key: string) => {
    setSelected((prev) => (prev.includes(key) ? prev.filter((k) => k !== key) : [...prev, key]));
  };

  async function submit() {
    if (submitting) return;
    if (form === "legacy") {
      // 旧会话不在这里建（首条消息懒创建）⇒ 切到草稿区必须给出**显式反馈**，
      // 否则右栏若本来就停在草稿区，用户会以为点了没反应。
      const agentName = agents.find((a) => a.key === legacyAgent)?.name ?? legacyAgent;
      onPickLegacy(legacyAgent);
      onOpenChange(false);
      toast.info(`旧会话（${agentName}）已就绪：在输入框发送第一条消息即创建`);
      return;
    }
    const members = selected.length > 0 ? selected : ["auto"];
    const trimmedGoal = goal.trim();
    if (form === "master" && trimmedGoal === "") {
      toast.error("总控会话需要填任务目标（也可以留空创建，之后在群里 @auto 说目标）");
      return;
    }
    setSubmitting(true);
    try {
      const created = await api.createSession({
        kind: form === "master" ? "master" : "chat",
        agents: members,
        dispatcher: members.includes("auto") ? "auto" : members[0],
        // 可选显式名（A4）：不填走三轨命名的自动档（首条人话生成标题）。
        ...(sessionName.trim() !== "" ? { name: sessionName.trim() } : {}),
        ...(form === "master" && trimmedGoal !== "" ? { goal: trimmedGoal } : {}),
      });
      onCreated(created);
      onOpenChange(false);
      if (form === "master") {
        toast.success(
          created.task_id ? `总控已创建：任务 #${created.task_id} 编排中，进展将播报进本会话` : "总控已创建",
        );
      } else {
        toast.success("群聊已创建：在下方输入消息，@ 成员即可唤醒它");
      }
    } catch (e) {
      toast.error(`新建失败：${(e as Error).message}`);
    } finally {
      setSubmitting(false);
    }
  }

  const busy = submitting;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>新建会话</DialogTitle>
          <DialogDescription>选择会话形态：三种形态承载不同的协作方式。</DialogDescription>
        </DialogHeader>
        <div className="grid gap-2">
          {(Object.keys(FORM_META) as FormKind[]).map((kind) => {
            const meta = FORM_META[kind];
            const active = form === kind;
            return (
              <button
                key={kind}
                type="button"
                onClick={() => setForm(kind)}
                className={`flex items-start gap-3 rounded-lg border p-3 text-left transition-colors ${
                  active ? "border-primary bg-accent" : "hover:bg-accent/50"
                }`}
              >
                <span className="mt-0.5 text-primary">{meta.icon}</span>
                <span className="min-w-0">
                  <span className="block font-medium text-sm">{meta.title}</span>
                  <span className="block text-muted-foreground text-xs">{meta.desc}</span>
                </span>
              </button>
            );
          })}
        </div>

        {form !== "legacy" ? (
          <div className="grid gap-2">
            <div className="grid gap-1.5">
              <label className="font-medium text-sm" htmlFor="session-name">
                会话名称（可选）
              </label>
              <Input
                id="session-name"
                value={sessionName}
                onChange={(e) => setSessionName(e.target.value)}
                placeholder="不填则用首条消息自动生成标题"
                className="h-8"
              />
            </div>
            <div className="font-medium text-sm">成员（{selected.length} 人，分发者默认 auto）</div>
            <div className="flex flex-wrap gap-1.5">
              {agents.map((a) => (
                <span
                  key={a.key}
                  className="flex items-center gap-1.5 rounded-md border px-2 py-1 text-xs has-[[data-state=checked]]:border-primary"
                >
                  <Checkbox
                    checked={selected.includes(a.key)}
                    onCheckedChange={() => toggleAgent(a.key)}
                    aria-label={`选择成员 ${a.name}`}
                  />
                  {a.name}
                </span>
              ))}
            </div>
          </div>
        ) : (
          <div className="grid gap-2">
            <div className="font-medium text-sm">对话对象</div>
            <select
              value={legacyAgent}
              onChange={(e) => setLegacyAgent(e.target.value)}
              className="rounded-md border bg-background px-2 py-1.5 text-sm"
            >
              {agents.map((a) => (
                <option key={a.key} value={a.key}>
                  {a.name}
                </option>
              ))}
            </select>
          </div>
        )}

        {form === "master" && (
          <div className="grid gap-2">
            <div className="font-medium text-sm">任务目标</div>
            <Textarea
              value={goal}
              onChange={(e) => setGoal(e.target.value)}
              placeholder="例：对 192.168.132.77 做端口探测，只针对这个 IP"
              rows={3}
            />
          </div>
        )}

        <div className="flex justify-end gap-2">
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            取消
          </Button>
          <Button onClick={() => void submit()} disabled={busy}>
            {busy && <Loader2Icon className="size-4 animate-spin" />}
            {form === "legacy" ? "去发第一条消息" : "创建"}
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  );
}
