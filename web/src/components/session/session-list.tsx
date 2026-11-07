"use client";

import * as React from "react";

import {
  Bot,
  ChevronRightIcon,
  Link2Icon,
  MoreHorizontalIcon,
  PencilIcon,
  PinIcon,
  PinOffIcon,
  Trash2Icon,
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
  // 收起中的分区（按 section.key，不带视图前缀）。折叠态由调用方持有：sections
  // 随列表轮询整体重建，状态不能挂在分区数组上。
  collapsedSections: ReadonlySet<string>;
  onToggleSection: (key: string) => void;
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

export interface SessionSectionHeaderProps {
  label: string;
  count: number;
  // 分区徽标（置顶 / 无成员 / 旧对话……），可缺省。
  icon?: React.ReactNode;
  collapsed: boolean;
  // 当前选中项是否落在这个分区里 —— 收起时据此亮小圆点。
  hasActive: boolean;
  onToggle: () => void;
  className?: string;
}

// SessionSectionHeader 是左栏一个分区的吸顶分区头：折叠箭头 · 徽标 · 名称 · 计数。
// 交互照抄旧对话的 AgentGroupHeader（chat/page.tsx）：整行是按钮、aria-expanded 报
// 展开态；收起时选中项若在组内亮一个小圆点，免得当前会话在列表里「消失」。
// 导出给 chat/page.tsx 的「旧对话」块复用，两处分区头长得一样。
export function SessionSectionHeader({
  label,
  count,
  icon,
  collapsed,
  hasActive,
  onToggle,
  className,
}: SessionSectionHeaderProps) {
  return (
    <button
      type="button"
      onClick={onToggle}
      aria-expanded={!collapsed}
      title={collapsed ? `展开「${label}」` : `收起「${label}」`}
      className={cn(
        "sticky top-0 z-10 flex min-w-0 items-center gap-1.5 rounded-md bg-card px-1.5 py-1 text-left font-medium text-[11px] transition-colors hover:bg-accent/50",
        collapsed && hasActive ? "text-foreground" : "text-muted-foreground",
        className,
      )}
    >
      <ChevronRightIcon className={cn("size-3 shrink-0 transition-transform", !collapsed && "rotate-90")} />
      {icon}
      <span className="min-w-0 flex-1 truncate">{label}</span>
      {collapsed && hasActive && (
        <span className="size-1.5 shrink-0 rounded-full bg-primary" title="当前选中项在此分组内" />
      )}
      <span className="shrink-0 tabular-nums opacity-60">{count}</span>
    </button>
  );
}

function sectionIcon(key: string): React.ReactNode {
  if (key === "pinned") return <PinIcon className="size-3 shrink-0" />;
  if (key === "__none__") return <Bot className="size-3 shrink-0" />;
  return null;
}

// SessionList 是左栏的会话列表：按分区渲染，每个分区一个可折叠的吸顶分区头 + 会话行。
// 分区由调用方算好（默认视图是 groupSessionsByKind 的总控 / 群聊 / 大厅 / 旧会话；
// 可选视图 groupSessionsByAgent / groupSessionsByAttachment），收起哪些分区也由调用方
// 决定（ChatPage 按视图前缀存 localStorage）—— 这里只管渲染与回调。
export function SessionList(props: SessionListProps) {
  // selectedId 不在此解构：渲染时用的是 props.selectedId（见下方 SessionRow 的 active）。
  const { sections, emptyHint, collapsedSections, onToggleSection } = props;
  if (sections.length === 0) {
    return <div className="px-2 py-6 text-center text-muted-foreground text-xs">{emptyHint ?? "暂无会话"}</div>;
  }
  return (
    <div className="flex min-w-0 flex-col gap-0.5">
      {sections.map((section) => {
        const collapsed = collapsedSections.has(section.key);
        return (
          <React.Fragment key={section.key}>
            <SessionSectionHeader
              label={section.label}
              count={section.sessions.length}
              icon={sectionIcon(section.key)}
              collapsed={collapsed}
              hasActive={section.sessions.some((session) => session.id === props.selectedId)}
              onToggle={() => onToggleSection(section.key)}
            />
            {!collapsed &&
              section.sessions.map((session) => (
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
        );
      })}
    </div>
  );
}
