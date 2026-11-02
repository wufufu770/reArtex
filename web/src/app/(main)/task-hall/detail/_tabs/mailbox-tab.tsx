"use client";

import * as React from "react";

import { SendIcon } from "lucide-react";
import { toast } from "sonner";

import { StatusBadge } from "@/components/status-badge";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type { TaskHallGroupDetail, TaskHallMessage, TaskNode } from "@/lib/types";

import { fmtDateTime, MESSAGE_KIND_LABEL } from "../../_components/task-hall-format";

const BROADCAST = "__all__";
const NO_INTENT = "__none__";

const KINDS = ["note", "request", "answer", "finding", "escalation"];

// MailboxTab 是群消息箱：人在这里插话，成员在各自会话里收取。
//
// 消息箱是**协商层**不是授权层：带 intent_id 的消息只是提议，不转移归属。
// 投递前的准入判定在存储层，被拦的消息照样落库留痕（状态 blocked + 拦截原因），
// 所以这里要把拦截原因原样显示出来，而不是当作发送失败。
export function MailboxTab({
  groupId,
  detail,
  messages,
  intents,
  onSent,
}: {
  groupId: number;
  detail: TaskHallGroupDetail;
  messages: TaskHallMessage[];
  intents: TaskNode[];
  onSent: () => void;
}) {
  const [toAgent, setToAgent] = React.useState(BROADCAST);
  const [kind, setKind] = React.useState("note");
  const [intentID, setIntentID] = React.useState(NO_INTENT);
  const [body, setBody] = React.useState("");
  const [sending, setSending] = React.useState(false);

  async function send() {
    const text = body.trim();
    if (!text) {
      toast.error("消息内容不能为空");
      return;
    }
    setSending(true);
    try {
      const sent = await api.postTaskHallMessage(groupId, {
        to_agent: toAgent === BROADCAST ? "" : toAgent,
        body: text,
        kind,
        intent_id: intentID === NO_INTENT ? undefined : Number(intentID),
      });
      if (sent.status === "blocked") {
        toast.error(`消息被拦截：${sent.blocked_reason || "未通过投递准入"}`, { duration: 12000 });
      } else {
        toast.success("已发到群消息箱");
      }
      setBody("");
      onSent();
    } catch (e) {
      toast.error(`发送失败：${(e as Error).message}`);
    } finally {
      setSending(false);
    }
  }

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <CardHeader>
          <CardTitle>插话</CardTitle>
          <CardDescription>
            人写进群消息箱的消息会被成员在各自会话里收取。消息是协商层：不改变意图归属，也不授予完成权。
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-3">
          <div className="grid gap-3 sm:grid-cols-3">
            <div className="grid gap-1.5">
              <Label>收件人</Label>
              <Select value={toAgent} onValueChange={setToAgent}>
                <SelectTrigger className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value={BROADCAST}>广播给全群</SelectItem>
                  {detail.members.map((m) => (
                    <SelectItem key={m.agent_key} value={m.agent_key}>
                      {m.agent_key}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="grid gap-1.5">
              <Label>类型</Label>
              <Select value={kind} onValueChange={setKind}>
                <SelectTrigger className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {KINDS.map((k) => (
                    <SelectItem key={k} value={k}>
                      {MESSAGE_KIND_LABEL[k] ?? k}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="grid gap-1.5">
              <Label>关联意图（可选）</Label>
              <Select value={intentID} onValueChange={setIntentID}>
                <SelectTrigger className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value={NO_INTENT}>不关联</SelectItem>
                  {intents.slice(0, 100).map((n) => (
                    <SelectItem key={n.id} value={n.id}>
                      #{n.id} · {n.state}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>
          <Textarea placeholder="写给群里的消息…" rows={3} value={body} onChange={(e) => setBody(e.target.value)} />
          <div>
            <Button onClick={() => void send()} disabled={sending || body.trim().length === 0}>
              {sending ? <Spinner data-icon="inline-start" /> : <SendIcon data-icon="inline-start" />}
              {sending ? "发送中" : "发送"}
            </Button>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>消息箱</CardTitle>
          <CardDescription>共 {messages.length} 条 · 含已读与被拦截的</CardDescription>
        </CardHeader>
        <CardContent>
          {messages.length === 0 ? (
            <Empty className="border border-dashed">
              <EmptyHeader>
                <EmptyTitle>还没有消息</EmptyTitle>
                <EmptyDescription>群里没人插话时这里就是空的。</EmptyDescription>
              </EmptyHeader>
            </Empty>
          ) : (
            <div className="flex flex-col gap-2">
              {messages.map((m) => (
                <div key={m.id} className="rounded-lg border p-3">
                  <div className="mb-1.5 flex flex-wrap items-center gap-2">
                    <span className="font-medium text-sm">{m.from_agent === "human" ? "人" : m.from_agent}</span>
                    <span className="text-muted-foreground text-xs">→ {m.to_agent === "" ? "全群" : m.to_agent}</span>
                    <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
                      {MESSAGE_KIND_LABEL[m.kind] ?? m.kind}
                    </Badge>
                    <StatusBadge domain="message" value={m.status} dot />
                    {m.intent_id ? (
                      <span className="font-mono text-muted-foreground text-xs">意图 #{m.intent_id}</span>
                    ) : null}
                    <span className="ml-auto text-muted-foreground text-xs">{fmtDateTime(m.created_at)}</span>
                  </div>
                  <p className="whitespace-pre-wrap text-sm">{m.body}</p>
                  {m.blocked_reason && (
                    <p className="mt-2 text-red-600 text-xs">
                      投递前被拦截：{m.blocked_reason}（已留痕，不进接收方上下文）
                    </p>
                  )}
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
