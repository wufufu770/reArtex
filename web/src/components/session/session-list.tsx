"use client";

import * as React from "react";

import { Bot, Link2Icon, MoreHorizontalIcon, PencilIcon, PinIcon, PinOffIcon, Trash2Icon } from "lucide-react";

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
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import type { Session } from "@/lib/session-types";
import type { Agent } from "@/lib/types";
import { cn } from "@/lib/utils";

import {
  relativeTime,
  type SessionSection,
  sessionDisplayName,
  sessionMembers,
  sessionStatus,
  sessionStatusMeta,
} from "./session-format";
import { RosterAvatars, RosterNames } from "./session-roster";

export interface SessionListProps {
  sections: SessionSection[];
  selectedId: number | null;
  onSelect: (id: number) => void;
  agentByKey: Map<string, Agent>;
  renamingId: number | null;
  renameText: string;
  onStartRename: (session: Session) => void;
  onRenameText: (value: string) => void;
  onCommitRename: (session: Session, name: string) => void;
  onCancelRename: () => void;
  onTogglePin: (session: Session) => void;
  onDelete: (session: Session) => void;
  emptyHint?: React.ReactNode;
}

function SessionRow({
  session,
  active,
  renaming,
  renameText,
  agentByKey,
  onSelect,
  onStartRename,
  onRenameText,
  onCommitRename,
  onCancelRename,
  onTogglePin,
  onDelete,
}: {
  session: Session;
  active: boolean;
  renaming: boolean;
  renameText: string;
  agentByKey: Map<string, Agent>;
  onSelect: (id: number) => void;
  onStartRename: (session: Session) => void;
  onRenameText: (value: string) => void;
  onCommitRename: (session: Session, name: string) => void;
  onCancelRename: () => void;
  onTogglePin: (session: Session) => void;
  onDelete: (session: Session) => void;
}) {
  const [deleteOpen, setDeleteOpen] = React.useState(false);
  const renameInputRef = React.useRef<HTMLInputElement>(null);
  const cancelRenameRef = React.useRef(false);
  const members = sessionMembers(session);
  const status = sessionStatusMeta(sessionStatus(session));
  const pinned = Boolean(session.pinned_at);
  const attachments = session.attachments ?? [];

  React.useEffect(() => {
    if (!renaming) return;
    cancelRenameRef.current = false;
    renameInputRef.current?.focus();
    renameInputRef.current?.select();
  }, [renaming]);

  return (
    <div
      className={cn(
        "group flex min-w-0 items-center gap-1 rounded-md pr-1 transition-colors",
        active ? "bg-accent text-accent-foreground" : "hover:bg-accent/50",
      )}
    >
      {renaming ? (
        <input
          ref={renameInputRef}
          value={renameText}
          onChange={(event) => onRenameText(event.target.value)}
          onBlur={() => {
            if (!cancelRenameRef.current) onCommitRename(session, renameText);
          }}
          onKeyDown={(event) => {
            if (event.key === "Enter") onCommitRename(session, renameText);
            if (event.key === "Escape") {
              event.preventDefault();
              cancelRenameRef.current = true;
              onCancelRename();
            }
          }}
          className="min-w-0 flex-1 rounded-md border border-input bg-background px-2 py-1 text-sm"
          aria-label="重命名会话"
        />
      ) : (
        <button
          type="button"
          onClick={() => onSelect(session.id)}
          onDoubleClick={() => onStartRename(session)}
          title="双击重命名"
          className="min-w-0 flex-1 rounded-md px-2 py-1.5 text-left"
        >
          <div className="flex min-w-0 items-center gap-1.5">
            {pinned && <PinIcon className="size-3 shrink-0 text-primary" aria-label="已置顶" />}
            {session.kind === "master" && (
              <span
                className="shrink-0 rounded border border-primary/40 px-1 font-medium text-primary text-[10px] leading-4"
                title="总控会话：绑定任务，进展自动播报进群"
              >
                总控
              </span>
            )}
            {session.kind === "task_hall" && (
              <span className="shrink-0 rounded border px-1 text-[10px] leading-4 text-muted-foreground">大厅</span>
            )}
            <span className="truncate text-sm">{sessionDisplayName(session)}</span>
            {status.label && <span className={cn("size-2 shrink-0 rounded-full", status.dot)} title={status.label} />}
          </div>
          <div className="flex min-w-0 items-center gap-1.5 text-[11px] text-muted-foreground">
            <RosterAvatars members={members} agentByKey={agentByKey} max={3} />
            <RosterNames members={members} agentByKey={agentByKey} max={2} className="min-w-0 truncate" />
            <span className="shrink-0 opacity-60">{relativeTime(session.updated_at)}</span>
            {attachments.length > 0 && (
              <Link2Icon className="size-3 shrink-0" aria-label={`关联 ${attachments.length} 个工件`} />
            )}
          </div>
        </button>
      )}
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            variant="ghost"
            size="icon-sm"
            className="shrink-0 text-muted-foreground"
            aria-label={`管理会话「${sessionDisplayName(session)}」`}
          >
            <MoreHorizontalIcon />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuGroup>
            <DropdownMenuItem onSelect={() => onStartRename(session)}>
              <PencilIcon />
              重命名
            </DropdownMenuItem>
            <DropdownMenuItem onSelect={() => onTogglePin(session)}>
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
      <AlertDialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除会话「{sessionDisplayName(session)}」？</AlertDialogTitle>
            <AlertDialogDescription>会话消息和成员记录将一并删除，此操作不可撤销。</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction onClick={() => onDelete(session)}>删除</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

// SessionList 是左栏的会话列表：扁平列表 + 置顶/今天/更早分区。
// 分区由调用方算好（groupSessions / groupSessionsByAgent / groupSessionsByAttachment）。
export function SessionList(props: SessionListProps) {
  // selectedId 不在此解构：渲染时用的是 props.selectedId（见下方 SessionRow 的 active）。
  const { sections, emptyHint } = props;
  if (sections.length === 0) {
    return <div className="px-2 py-6 text-center text-muted-foreground text-xs">{emptyHint ?? "暂无会话"}</div>;
  }
  return (
    <div className="flex min-w-0 flex-col gap-0.5">
      {sections.map((section) => (
        <React.Fragment key={section.key}>
          <div className="flex items-center gap-1.5 px-2 py-1 font-medium text-[11px] text-muted-foreground">
            {section.key === "pinned" && <PinIcon className="size-3 shrink-0" />}
            {section.key === "__none__" && <Bot className="size-3 shrink-0" />}
            <span className="truncate">{section.label}</span>
            <span className="shrink-0 tabular-nums opacity-60">{section.sessions.length}</span>
          </div>
          {section.sessions.map((session) => (
            <SessionRow
              key={session.id}
              session={session}
              active={props.selectedId === session.id}
              renaming={props.renamingId === session.id}
              renameText={props.renamingId === session.id ? props.renameText : ""}
              agentByKey={props.agentByKey}
              onSelect={props.onSelect}
              onStartRename={props.onStartRename}
              onRenameText={props.onRenameText}
              onCommitRename={props.onCommitRename}
              onCancelRename={props.onCancelRename}
              onTogglePin={props.onTogglePin}
              onDelete={props.onDelete}
            />
          ))}
        </React.Fragment>
      ))}
    </div>
  );
}
