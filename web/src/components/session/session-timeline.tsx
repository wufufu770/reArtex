"use client";

import * as React from "react";

import { Bot, CheckCheckIcon, ChevronDownIcon, ChevronRightIcon, UserIcon } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { MessageScrollerItem } from "@/components/ui/message-scroller";
import type { SessionMessage, SessionMessageKind } from "@/lib/session-types";
import type { Agent } from "@/lib/types";
import { cn } from "@/lib/utils";

const KIND_LABELS: Record<SessionMessageKind, string> = {
  note: "",
  request: "请求",
  answer: "回答",
  finding: "发现",
  escalation: "升级",
  receipt: "收据",
};

const RECEIPT_LABELS: Record<string, string> = {
  taken: "已接收",
  noted: "已记下",
  deferred: "已推迟",
};

function agentName(key: string, agentByKey: Map<string, Agent>): string {
  if (!key) return "人";
  return agentByKey.get(key)?.name || key;
}

function MessageMeta({
  message,
  agentByKey,
  className,
}: {
  message: SessionMessage;
  agentByKey: Map<string, Agent>;
  className?: string;
}) {
  const kind = KIND_LABELS[message.kind];
  return (
    <div className={cn("flex min-w-0 items-center gap-1.5 text-[11px] text-muted-foreground", className)}>
      {kind && (
        <Badge variant="secondary" className="shrink-0 px-1 py-0 text-[9px]">
          {kind}
        </Badge>
      )}
      {message.to_agent && <span className="shrink-0">@{agentName(message.to_agent, agentByKey)}</span>}
      <time dateTime={message.created_at} className="shrink-0 tabular-nums opacity-70">
        {new Date(message.created_at).toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit" })}
      </time>
    </div>
  );
}

function ReceiptStrip({ receipts, agentByKey }: { receipts: SessionMessage[]; agentByKey: Map<string, Agent> }) {
  if (receipts.length === 0) return null;
  return (
    <div className="mt-1 flex flex-wrap items-center gap-2 pl-1">
      {receipts.map((receipt) => (
        <span
          key={receipt.id}
          className="flex items-center gap-1 text-[11px] text-muted-foreground"
          title={`${agentName(receipt.from_agent, agentByKey)} · ${RECEIPT_LABELS[receipt.receipt_action] ?? "已读"}`}
        >
          <CheckCheckIcon className="size-3 text-emerald-500" />
          {agentName(receipt.from_agent, agentByKey)}
          {RECEIPT_LABELS[receipt.receipt_action] ? ` ${RECEIPT_LABELS[receipt.receipt_action]}` : ""}
        </span>
      ))}
    </div>
  );
}

function MessageBubble({
  message,
  agentByKey,
  replies,
  receipts,
  compact,
}: {
  message: SessionMessage;
  agentByKey: Map<string, Agent>;
  replies: SessionMessage[];
  receipts: SessionMessage[];
  compact?: boolean;
}) {
  const [open, setOpen] = React.useState(false);
  const human = !message.from_agent;
  const name = human ? "我" : agentName(message.from_agent, agentByKey);

  const body = (
    <>
      <div
        className={cn(
          "min-w-0 max-w-full whitespace-pre-wrap rounded-lg px-3 py-1.5 text-sm [overflow-wrap:anywhere]",
          human ? "rounded-tr-sm bg-primary text-primary-foreground" : "bg-muted text-card-foreground",
          compact && "px-2 py-1 text-xs",
        )}
      >
        {message.body}
      </div>
      {message.blocked_reason && (
        <p className="text-red-600 text-xs dark:text-red-400">已拦截：{message.blocked_reason}</p>
      )}
    </>
  );

  return (
    <div className={cn("flex min-w-0 gap-2", human ? "justify-end" : "justify-start")}>
      {!human && (
        <div className="mt-0.5 flex size-6 shrink-0 items-center justify-center rounded-full bg-primary/10">
          <Bot className="size-3.5 text-primary" />
        </div>
      )}
      <div className={cn("flex min-w-0 max-w-[85%] flex-col", human ? "items-end" : "items-start")}>
        {!human && <span className="mb-0.5 font-medium text-[11px] text-muted-foreground">{name}</span>}
        {body}
        <MessageMeta message={message} agentByKey={agentByKey} className={human ? "mt-0.5" : "mt-0.5"} />
        <ReceiptStrip receipts={receipts} agentByKey={agentByKey} />
        {replies.length > 0 && (
          <Collapsible open={open} onOpenChange={setOpen} className="mt-1 w-full">
            <CollapsibleTrigger className="flex items-center gap-1 text-[11px] text-muted-foreground hover:text-foreground">
              {open ? <ChevronDownIcon className="size-3" /> : <ChevronRightIcon className="size-3" />}
              {replies.length} 条补充
            </CollapsibleTrigger>
            <CollapsibleContent className="mt-1 flex flex-col gap-1.5 border-l pl-2">
              {replies.map((reply) => (
                <MessageBubble
                  key={reply.id}
                  message={reply}
                  agentByKey={agentByKey}
                  replies={[]}
                  receipts={[]}
                  compact
                />
              ))}
            </CollapsibleContent>
          </Collapsible>
        )}
      </div>
      {human && (
        <div className="mt-0.5 flex size-6 shrink-0 items-center justify-center rounded-full bg-primary/10">
          <UserIcon className="size-3.5 text-primary" />
        </div>
      )}
    </div>
  );
}

// SessionTimeline 渲染会话的共享消息流：人 / agent 同一条流。
//
// 两条渲染规则来自方案书 §4.1 / §2.3：
//   · receipt_of 非空 ⇒ 收据条，贴在原消息下面，**不占气泡**；
//   · parent_message_id 非空 ⇒ 线程回复，折叠在原消息下面。
//
// 必须渲染在 MessageScrollerContent 里（见 SessionView）：每条顶层消息是一个
// MessageScrollerItem，直接挂在 Content 下、中间不再包容器 —— 滚动器靠 Content 的
// 直接子节点变化感知「来了新消息」（贴底跟随、打开落底）。
export function SessionTimeline({
  messages,
  agentByKey,
  emptyHint,
}: {
  messages: SessionMessage[];
  agentByKey: Map<string, Agent>;
  emptyHint?: React.ReactNode;
}) {
  const repliesByParent = React.useMemo(() => {
    const map = new Map<number, SessionMessage[]>();
    for (const message of messages) {
      if (message.receipt_of != null) continue;
      if (message.parent_message_id == null) continue;
      const list = map.get(message.parent_message_id) ?? [];
      list.push(message);
      map.set(message.parent_message_id, list);
    }
    return map;
  }, [messages]);

  const receiptsByTarget = React.useMemo(() => {
    const map = new Map<number, SessionMessage[]>();
    for (const message of messages) {
      if (message.receipt_of == null) continue;
      const list = map.get(message.receipt_of) ?? [];
      list.push(message);
      map.set(message.receipt_of, list);
    }
    return map;
  }, [messages]);

  const topLevel = React.useMemo(
    () => messages.filter((message) => message.receipt_of == null && message.parent_message_id == null),
    [messages],
  );

  if (messages.length === 0) {
    return (
      <div className="flex min-h-0 flex-1 items-center justify-center px-4 py-10 text-center text-muted-foreground text-sm">
        {emptyHint ?? "这个会话还没有消息"}
      </div>
    );
  }

  return topLevel.map((message) => (
    <MessageScrollerItem key={message.id} messageId={String(message.id)}>
      <MessageBubble
        message={message}
        agentByKey={agentByKey}
        replies={repliesByParent.get(message.id) ?? []}
        receipts={receiptsByTarget.get(message.id) ?? []}
      />
    </MessageScrollerItem>
  ));
}
