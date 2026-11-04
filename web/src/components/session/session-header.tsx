"use client";

import * as React from "react";

import {
  Bot,
  Link2Icon,
  MoreHorizontalIcon,
  PencilIcon,
  PinIcon,
  PinOffIcon,
  PlusIcon,
  Trash2Icon,
  UserMinusIcon,
} from "lucide-react";

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import type { Session, SessionMember } from "@/lib/session-types";
import type { Agent } from "@/lib/types";
import { cn, getInitials } from "@/lib/utils";

import { memberDisplayName, presenceMeta, sessionDisplayName, sessionNameSourceLabel } from "./session-format";
import { PresenceDot } from "./session-roster";

export interface SessionHeaderProps {
  session: Session;
  members: SessionMember[];
  agentByKey: Map<string, Agent>;
  // 可加入的 agent 候选（不在席上的对话型 agent）。
  candidates: Agent[];
  onRename: (name: string) => void;
  onTogglePin: () => void;
  onDelete: () => void;
  onAddMember: (agentKey: string) => void;
  onRemoveMember: (agentKey: string) => void;
  // 更新成员在场状态（PATCH /sessions/{id}/members/{agent}）；未提供时状态菜单不出现。
  onSetPresence?: (agentKey: string, presence: string) => void;
  // 会话流 / 旧对话记录 的切换条（由调用方渲染）。
  tabs?: React.ReactNode;
  // 右侧抽屉的触发按钮。
  aside?: React.ReactNode;
  busy?: boolean;
}

const KIND_LABELS: Record<string, string> = {
  task_hall: "任务大厅",
  side_question: "旁路",
};

// SessionHeader 是会话页顶部：会话名（可改）+ agent 花名册（presence 状态点）+ 操作。
export function SessionHeader({
  session,
  members,
  agentByKey,
  candidates,
  onRename,
  onTogglePin,
  onDelete,
  onAddMember,
  onRemoveMember,
  onSetPresence,
  tabs,
  aside,
  busy,
}: SessionHeaderProps) {
  const [editing, setEditing] = React.useState(false);
  const [draft, setDraft] = React.useState(session.name);
  const [deleteOpen, setDeleteOpen] = React.useState(false);
  const inputRef = React.useRef<HTMLInputElement>(null);
  const pinned = Boolean(session.pinned_at);

  React.useEffect(() => {
    if (!editing) return;
    setDraft(session.name);
    inputRef.current?.focus();
    inputRef.current?.select();
  }, [editing, session.name]);

  function commit() {
    const value = draft.trim();
    setEditing(false);
    if (!value || value === session.name) return;
    onRename(value);
  }

  return (
    <div className="flex min-w-0 flex-col gap-2 border-b px-4 py-2.5">
      <div className="flex min-w-0 items-center gap-2">
        {editing ? (
          <input
            ref={inputRef}
            value={draft}
            onChange={(event) => setDraft(event.target.value)}
            onBlur={commit}
            onKeyDown={(event) => {
              if (event.key === "Enter") commit();
              if (event.key === "Escape") setEditing(false);
            }}
            className="min-w-0 max-w-72 flex-1 rounded-md border border-input bg-background px-2 py-1 text-sm"
            aria-label="会话名"
          />
        ) : (
          <button
            type="button"
            onDoubleClick={() => setEditing(true)}
            title="双击重命名"
            className="min-w-0 max-w-72 truncate text-left font-medium text-sm hover:underline"
          >
            {sessionDisplayName(session)}
          </button>
        )}
        {session.kind !== "chat" && KIND_LABELS[session.kind] && (
          <Badge variant="outline" className="shrink-0 px-1.5 py-0 text-[10px]">
            {KIND_LABELS[session.kind]}
          </Badge>
        )}
        <Badge variant="secondary" className="shrink-0 px-1.5 py-0 text-[10px]">
          {sessionNameSourceLabel(session)}
        </Badge>
        <div className="ml-auto flex min-w-0 items-center gap-1.5">
          {tabs}
          {aside}
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="ghost" size="icon-sm" className="text-muted-foreground" aria-label="会话操作">
                <MoreHorizontalIcon />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuGroup>
                <DropdownMenuItem onSelect={() => setEditing(true)}>
                  <PencilIcon />
                  重命名
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={onTogglePin}>
                  {pinned ? <PinOffIcon /> : <PinIcon />}
                  {pinned ? "取消置顶" : "置顶"}
                </DropdownMenuItem>
              </DropdownMenuGroup>
              <DropdownMenuSeparator />
              <DropdownMenuGroup>
                <DropdownMenuItem variant="destructive" onSelect={() => setDeleteOpen(true)}>
                  <Trash2Icon />
                  删除
                </DropdownMenuItem>
              </DropdownMenuGroup>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>

      <div className="flex min-w-0 flex-wrap items-center gap-1.5">
        {members.length === 0 && <span className="text-muted-foreground text-xs">暂无成员</span>}
        {members.map((member) => {
          const meta = presenceMeta(member.presence);
          const name = memberDisplayName(member, agentByKey);
          return (
            <DropdownMenu key={member.agent_key}>
              <DropdownMenuTrigger asChild>
                <button
                  type="button"
                  className={cn(
                    "flex min-w-0 items-center gap-1.5 rounded-md border px-1.5 py-0.5 text-xs hover:bg-accent/50",
                    member.left_at && "opacity-60",
                  )}
                  title={`${name} · ${meta.label}${member.role === "dispatcher" ? " · 分发者" : ""}`}
                >
                  <span className="flex size-4 shrink-0 items-center justify-center rounded-full bg-muted text-[9px]">
                    {getInitials(name)}
                  </span>
                  <PresenceDot presence={member.presence} />
                  <span className="max-w-28 truncate">{name}</span>
                </button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="start">
                {onSetPresence && !member.left_at && (
                  <DropdownMenuGroup>
                    <DropdownMenuItem onSelect={() => onSetPresence(member.agent_key, "listening")}>
                      在听
                    </DropdownMenuItem>
                    <DropdownMenuItem onSelect={() => onSetPresence(member.agent_key, "working")}>
                      工作中
                    </DropdownMenuItem>
                    <DropdownMenuItem onSelect={() => onSetPresence(member.agent_key, "blocked")}>
                      需关注（阻塞）
                    </DropdownMenuItem>
                    <DropdownMenuItem onSelect={() => onSetPresence(member.agent_key, "idle")}>
                      在席（空闲）
                    </DropdownMenuItem>
                  </DropdownMenuGroup>
                )}
                {onSetPresence && !member.left_at && <DropdownMenuSeparator />}
                <DropdownMenuGroup>
                  <DropdownMenuItem
                    disabled={member.role === "dispatcher"}
                    onSelect={() => onRemoveMember(member.agent_key)}
                  >
                    <UserMinusIcon />
                    移出会话
                  </DropdownMenuItem>
                </DropdownMenuGroup>
              </DropdownMenuContent>
            </DropdownMenu>
          );
        })}
        <Popover>
          <PopoverTrigger asChild>
            <Button size="sm" variant="ghost" className="h-6 px-1.5 text-xs" disabled={busy}>
              <PlusIcon />
              添加 agent
            </Button>
          </PopoverTrigger>
          <PopoverContent align="start" className="w-56 p-1">
            <p className="px-2 py-1 font-medium text-[11px] text-muted-foreground">加入会话</p>
            {candidates.length === 0 && <p className="px-2 py-1.5 text-muted-foreground text-xs">没有可加入的 agent</p>}
            {candidates.map((agent) => (
              <button
                key={agent.key}
                type="button"
                disabled={busy}
                onClick={() => onAddMember(agent.key)}
                className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-left text-sm hover:bg-accent disabled:opacity-50"
              >
                <Bot className="size-3.5 shrink-0" />
                <span className="min-w-0 flex-1 truncate">{agent.name}</span>
                <span className="shrink-0 text-[11px] text-muted-foreground">{agent.key}</span>
              </button>
            ))}
          </PopoverContent>
        </Popover>
        {(session.attachments ?? []).length > 0 && (
          <span className="flex items-center gap-1 text-[11px] text-muted-foreground">
            <Link2Icon className="size-3" />
            {session.attachments?.length} 个关联工件
          </span>
        )}
      </div>

      <AlertDialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除会话「{sessionDisplayName(session)}」？</AlertDialogTitle>
            <AlertDialogDescription>会话消息和成员记录将一并删除，此操作不可撤销。</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction onClick={onDelete}>删除</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
