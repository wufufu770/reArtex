"use client";

import * as React from "react";

import { ArrowUpIcon, Loader2Icon, PaperclipIcon, Square, XIcon } from "lucide-react";

import { MentionTextarea } from "@/components/mention-textarea";
import { Button } from "@/components/ui/button";
import type { ChatMention } from "@/lib/chat-mentions";
import { shouldSubmitOnKey, useChatSendMode } from "@/lib/chat-send-mode";
import type { SessionMember } from "@/lib/session-types";
import type { Agent, ChatAttachment } from "@/lib/types";

import { memberDisplayName, presenceMeta } from "./session-format";

export interface SessionComposerProps {
  members: SessionMember[];
  // 可加入的 agent（不在席上的对话型 agent）—— 在 / 面板里点到它们时先热加入再定向。
  candidates: Agent[];
  agentByKey: Map<string, Agent>;
  onSend: (body: string, toAgent: string) => void | Promise<void>;
  onPickFiles?: (files: File[]) => void;
  attachments?: ChatAttachment[];
  onRemoveAttachment?: (path: string) => void;
  uploading?: boolean;
  disabled?: boolean;
  running?: boolean;
  onStop?: () => void;
}

// resolveTarget 从正文里解析收件人：最后一个裸 @词。@everyone ⇒ 广播（空串）。
// 记录的结构化 token（@[漏洞#12 …]）不是人名，要跳过，否则它会把收件人吃掉。
function resolveTarget(body: string, members: SessionMember[], agentByKey: Map<string, Agent>): string {
  const matches = [...body.matchAll(/@([^\s@[\]]+)/g)];
  const last = matches.at(-1);
  if (!last) return "";
  const token = (last[1] ?? "").toLowerCase();
  if (token === "everyone") return "";
  const member = members.find((candidate) => {
    const name = memberDisplayName(candidate, agentByKey).toLowerCase();
    return candidate.agent_key.toLowerCase() === token || name === token;
  });
  if (member) return member.agent_key;
  const agent = [...agentByKey.values()].find(
    (candidate) => candidate.key.toLowerCase() === token || candidate.name.toLowerCase() === token,
  );
  return agent ? agent.key : "";
}

// SessionComposer 是会话页的输入框：/ 唤出 agent 选择器与记录引用，@everyone 广播，
// 附件随消息一起发。
export function SessionComposer({
  members,
  candidates,
  agentByKey,
  onSend,
  onPickFiles,
  attachments,
  onRemoveAttachment,
  uploading,
  disabled,
  running,
  onStop,
}: SessionComposerProps) {
  const [value, setValue] = React.useState("");
  const [sending, setSending] = React.useState(false);
  const fileInputRef = React.useRef<HTMLInputElement>(null);
  const sendMode = useChatSendMode();
  const atts = attachments ?? [];

  const extraItems = React.useMemo<ChatMention[]>(() => {
    const seated = new Set(members.map((member) => member.agent_key));
    const list: ChatMention[] = [
      { kind: "agent", id: -1, label: "everyone", description: "广播给全会话", agentKey: "everyone" },
    ];
    for (const member of members) {
      list.push({
        kind: "agent",
        id: -1,
        label: memberDisplayName(member, agentByKey),
        description: presenceMeta(member.presence).label,
        agentKey: member.agent_key,
      });
    }
    for (const agent of candidates) {
      if (seated.has(agent.key)) continue;
      list.push({
        kind: "agent",
        id: -1,
        label: agent.name,
        description: "加入并呼叫",
        agentKey: agent.key,
      });
    }
    return list;
  }, [members, candidates, agentByKey]);

  async function send() {
    const body = value.trim();
    if ((!body && atts.length === 0) || sending || disabled) return;
    setSending(true);
    try {
      await onSend(body, resolveTarget(body, members, agentByKey));
      setValue("");
    } finally {
      setSending(false);
    }
  }

  function onKeyDown(event: React.KeyboardEvent<HTMLTextAreaElement>) {
    if (!shouldSubmitOnKey(event, sendMode)) return;
    event.preventDefault();
    void send();
  }

  return (
    <div className="border-t p-3">
      {atts.length > 0 && (
        <div className="mb-2 flex flex-wrap gap-1.5">
          {atts.map((attachment) => (
            <div
              key={attachment.path}
              className="flex items-center gap-1.5 rounded-md border bg-muted/50 px-2 py-1 text-xs"
              title={attachment.path}
            >
              <PaperclipIcon className="size-3 shrink-0 text-primary" />
              <span className="max-w-[160px] truncate">{attachment.name}</span>
              {onRemoveAttachment && (
                <button
                  type="button"
                  className="text-muted-foreground hover:text-foreground"
                  onClick={() => onRemoveAttachment(attachment.path)}
                  title="移除"
                >
                  <XIcon className="size-3" />
                </button>
              )}
            </div>
          ))}
        </div>
      )}
      <div className="flex flex-wrap items-center gap-2">
        {onPickFiles && (
          <>
            <input
              ref={fileInputRef}
              type="file"
              multiple
              className="hidden"
              onChange={(event) => {
                const picked = Array.from(event.target.files ?? []);
                event.target.value = "";
                if (picked.length > 0) onPickFiles(picked);
              }}
            />
            <Button
              size="icon"
              variant="ghost"
              onClick={() => fileInputRef.current?.click()}
              // biome-ignore lint/nursery/useNullishCoalescing: disabled 与 uploading 都是布尔量，|| 才是「任一为真即置灰」；改成 ?? 会让上传中不置灰。
              disabled={disabled || uploading}
              title="上传文件"
              aria-label="上传文件"
            >
              {uploading ? <Loader2Icon className="size-4 animate-spin" /> : <PaperclipIcon className="size-4" />}
            </Button>
          </>
        )}
        <MentionTextarea
          className="max-h-40 min-h-10 min-w-0 flex-1 resize-none"
          rows={1}
          value={value}
          disabled={disabled}
          onValueChange={setValue}
          onKeyDown={onKeyDown}
          trigger="/"
          extraItems={extraItems}
          placeholder="输入消息，/ 选择成员或引用记录，Enter 发送"
          aria-label="会话消息"
        />
        {running ? (
          <Button size="icon" variant="destructive" onClick={onStop} title="停止" aria-label="停止">
            <Square className="size-3.5 fill-current" />
          </Button>
        ) : (
          <Button
            size="icon"
            onClick={() => void send()}
            // biome-ignore lint/nursery/useNullishCoalescing: 同上 —— 全是布尔量，|| 表达「任一为真即置灰」。
            disabled={disabled || sending || (!value.trim() && atts.length === 0)}
            title="发送消息"
            aria-label="发送消息"
          >
            <ArrowUpIcon />
          </Button>
        )}
      </div>
    </div>
  );
}
