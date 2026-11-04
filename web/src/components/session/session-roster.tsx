"use client";

import { Avatar, AvatarBadge, AvatarFallback, AvatarGroup, AvatarGroupCount } from "@/components/ui/avatar";
import type { SessionMember } from "@/lib/session-types";
import type { Agent } from "@/lib/types";
import { getInitials } from "@/lib/utils";

import { memberDisplayName, presenceMeta } from "./session-format";

// PresenceDot 是成员名册上的那一个状态点：在席 / 在听 / 工作中 / 需关注 / 已退出。
export function PresenceDot({ presence, className }: { presence: string; className?: string }) {
  const meta = presenceMeta(presence);
  return (
    <span
      role="img"
      className={`inline-block size-2 shrink-0 rounded-full ring-1 ring-background ${meta.dot} ${className ?? ""}`}
      title={meta.label}
      aria-label={meta.label}
    />
  );
}

// RosterAvatars 是会话名册的头像组（列表行与头部共用）：最多 max 个头像，
// 多出来的用数量角标表示。头像角标是成员的状态点。
export function RosterAvatars({
  members,
  agentByKey,
  max = 3,
  className,
}: {
  members: SessionMember[];
  agentByKey: Map<string, Agent>;
  max?: number;
  className?: string;
}) {
  if (members.length === 0) {
    return <span className="text-[11px] text-muted-foreground">暂无成员</span>;
  }
  const shown = members.slice(0, max);
  const rest = members.length - shown.length;
  return (
    <AvatarGroup className={className}>
      {shown.map((member) => {
        const meta = presenceMeta(member.presence);
        const name = memberDisplayName(member, agentByKey);
        return (
          <Avatar key={member.agent_key} size="sm" title={`${name} · ${meta.label}`}>
            <AvatarFallback className="text-[10px]">{getInitials(name)}</AvatarFallback>
            <AvatarBadge className={meta.dot} />
          </Avatar>
        );
      })}
      {rest > 0 && <AvatarGroupCount className="size-6 text-[10px]">+{rest}</AvatarGroupCount>}
    </AvatarGroup>
  );
}

// RosterNames 把名册渲染成一行文本（列表行里的「auto, pentest」那种）。
export function RosterNames({
  members,
  agentByKey,
  max = 3,
  className,
}: {
  members: SessionMember[];
  agentByKey: Map<string, Agent>;
  max?: number;
  className?: string;
}) {
  if (members.length === 0) return <span className="text-[11px] text-muted-foreground">暂无成员</span>;
  const names = members.slice(0, max).map((member) => memberDisplayName(member, agentByKey));
  const rest = members.length - names.length;
  return (
    <span className={className} title={members.map((m) => memberDisplayName(m, agentByKey)).join(", ")}>
      {names.join(", ")}
      {rest > 0 ? ` +${rest}` : ""}
    </span>
  );
}
