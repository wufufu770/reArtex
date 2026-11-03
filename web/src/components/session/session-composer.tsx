"use client";

import * as React from "react";

import { ArrowUpIcon, AtSignIcon, Loader2Icon, PaperclipIcon, Square, XIcon } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { shouldSubmitOnKey, useChatSendMode } from "@/lib/chat-send-mode";
import type { SessionMember } from "@/lib/session-types";
import type { Agent, ChatAttachment } from "@/lib/types";
import { cn } from "@/lib/utils";

import { memberDisplayName, presenceMeta } from "./session-format";

export interface SessionComposerProps {
  members: SessionMember[];
  // 可加入的 agent（不在席上的对话型 agent）—— @ 到它们时先热加入再定向。
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

interface MentionTarget {
  key: string;
  label: string;
  hint: string;
  onSeat: boolean;
}

// detectMention 找光标前最后一个 @词（还没跟空格 ⇒ 还在输入中）。
function detectMention(value: string, caret: number): { query: string; start: number; caret: number } | null {
  const upto = value.slice(0, caret);
  const match = upto.match(/@([^\s@]*)$/);
  if (!match) return null;
  return { query: match[1], start: caret - match[0].length, caret };
}

// resolveTarget 从正文里解析收件人：最后一个 @词。@everyone ⇒ 广播（空串）。
function resolveTarget(body: string, members: SessionMember[], agentByKey: Map<string, Agent>): string {
  const matches = [...body.matchAll(/@([^\s@]+)/g)];
  const last = matches.at(-1);
  if (!last) return "";
  const token = (last[1] ?? "").toLowerCase();
  if (token === "everyone") return "";
  const member = members.find((candidate) => {
    const name = memberDisplayName(candidate, agentByKey).toLowerCase();
    return candidate.agent_key.toLowerCase() === token || name === token;
  });
  if (member) return member.agent_key;
  const agent = [...agentByKey.values()].find((candidate) => candidate.name.toLowerCase() === token);
  return agent ? agent.key : "";
}

// SessionComposer 是会话页的输入框：@ 唤出 agent 选择器，@everyone 广播，
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
  const [mention, setMention] = React.useState<{ query: string; start: number; caret: number } | null>(null);
  const textareaRef = React.useRef<HTMLTextAreaElement>(null);
  const fileInputRef = React.useRef<HTMLInputElement>(null);
  const sendMode = useChatSendMode();
  const atts = attachments ?? [];

  const targets: MentionTarget[] = React.useMemo(() => {
    const seated = new Set(members.map((member) => member.agent_key));
    const list: MentionTarget[] = [];
    list.push({ key: "everyone", label: "everyone", hint: "广播给全会话", onSeat: false });
    for (const member of members) {
      list.push({
        key: member.agent_key,
        label: memberDisplayName(member, agentByKey),
        hint: presenceMeta(member.presence).label,
        onSeat: true,
      });
    }
    for (const agent of candidates) {
      if (seated.has(agent.key)) continue;
      list.push({ key: agent.key, label: agent.name, hint: "加入并呼叫", onSeat: false });
    }
    return list;
  }, [members, candidates, agentByKey]);

  const filtered = React.useMemo(() => {
    const query = mention?.query.toLowerCase() ?? "";
    if (!query) return targets;
    return targets.filter((target) => target.label.toLowerCase().includes(query));
  }, [mention, targets]);

  function applyMention(target: MentionTarget) {
    if (!mention) return;
    const before = value.slice(0, mention.start);
    const after = value.slice(mention.caret);
    const inserted = `@${target.label} `;
    const next = `${before}${inserted}${after}`;
    setValue(next);
    setMention(null);
    requestAnimationFrame(() => {
      const el = textareaRef.current;
      if (!el) return;
      const caret = before.length + inserted.length;
      el.focus();
      el.setSelectionRange(caret, caret);
    });
  }

  async function send() {
    const body = value.trim();
    if ((!body && atts.length === 0) || sending || disabled) return;
    setSending(true);
    try {
      await onSend(body, resolveTarget(body, members, agentByKey));
      setValue("");
      setMention(null);
    } finally {
      setSending(false);
    }
  }

  function onKeyDown(event: React.KeyboardEvent<HTMLTextAreaElement>) {
    if (mention && filtered.length > 0) {
      if (event.key === "ArrowDown" || event.key === "Enter" || event.key === "Tab") {
        event.preventDefault();
        applyMention(filtered[0]);
        return;
      }
      if (event.key === "Escape") {
        event.preventDefault();
        setMention(null);
        return;
      }
    }
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
              <PaperclipIcon className="text-primary size-3 shrink-0" />
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
      {mention && filtered.length > 0 && (
        <div className="bg-popover mb-2 max-h-56 overflow-y-auto rounded-md border p-1 shadow-md">
          {filtered.map((target) => (
            <button
              key={target.key}
              type="button"
              onMouseDown={(event) => {
                event.preventDefault();
                applyMention(target);
              }}
              className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-left text-sm hover:bg-accent"
            >
              <AtSignIcon className="text-muted-foreground size-3.5 shrink-0" />
              <span className="min-w-0 flex-1 truncate">{target.label}</span>
              <span className="text-muted-foreground shrink-0 text-[11px]">{target.hint}</span>
            </button>
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
              disabled={disabled || uploading}
              title="上传文件"
              aria-label="上传文件"
            >
              {uploading ? <Loader2Icon className="size-4 animate-spin" /> : <PaperclipIcon className="size-4" />}
            </Button>
          </>
        )}
        <Textarea
          ref={textareaRef}
          rows={1}
          value={value}
          disabled={disabled}
          onChange={(event) => {
            const next = event.target.value;
            setValue(next);
            const caret = event.target.selectionStart ?? next.length;
            setMention(detectMention(next, caret));
          }}
          onKeyDown={onKeyDown}
          onBlur={() => setMention(null)}
          onClick={(event) => {
            const caret = event.currentTarget.selectionStart ?? value.length;
            setMention(detectMention(value, caret));
          }}
          placeholder="输入消息，@ 选择 agent，Enter 发送"
          aria-label="会话消息"
          className={cn("max-h-40 min-h-10 min-w-0 flex-1 resize-none")}
        />
        {running ? (
          <Button size="icon" variant="destructive" onClick={onStop} title="停止" aria-label="停止">
            <Square className="size-3.5 fill-current" />
          </Button>
        ) : (
          <Button
            size="icon"
            onClick={() => void send()}
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
