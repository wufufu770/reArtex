"use client";

import { Link2Icon, UsersIcon } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Separator } from "@/components/ui/separator";
import type { Session, SessionMember } from "@/lib/session-types";
import type { Agent, TaskHallQuota } from "@/lib/types";

import { attachmentTypeLabel, memberDisplayName, presenceMeta } from "./session-format";
import { PresenceDot } from "./session-roster";

const ROLE_LABELS: Record<string, string> = {
  dispatcher: "分发者",
  member: "成员",
  observer: "观察者",
};

function fmtTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1000) return `${(n / 1000).toFixed(1)}k`;
  return String(n);
}

// SessionSidePanel 是会话页右侧抽屉的内容：关联的工件 + 参与 agent 的状态与配额。
export function SessionSidePanel({
  session,
  members,
  agentByKey,
  quota,
}: {
  session: Session;
  members: SessionMember[];
  agentByKey: Map<string, Agent>;
  quota: TaskHallQuota | null;
}) {
  const attachments = session.attachments ?? [];
  return (
    <div className="flex flex-col gap-4 p-4">
      <section className="flex flex-col gap-2">
        <h3 className="flex items-center gap-1.5 font-medium text-xs">
          <Link2Icon className="size-3.5" />
          关联工件
        </h3>
        {attachments.length === 0 ? (
          <p className="text-muted-foreground text-xs">未关联任务 / 策略 / 资产</p>
        ) : (
          <ul className="flex flex-col gap-1.5">
            {attachments.map((attachment) => (
              <li
                key={`${attachment.attachment_type}:${attachment.identity_key}`}
                className="flex min-w-0 items-center gap-2 rounded-md border px-2 py-1.5"
              >
                <Badge variant="outline" className="shrink-0 px-1 py-0 text-[10px]">
                  {attachmentTypeLabel(attachment.attachment_type)}
                </Badge>
                <span className="min-w-0 flex-1 truncate text-xs" title={attachment.identity_key}>
                  {attachment.identity_key}
                </span>
              </li>
            ))}
          </ul>
        )}
      </section>

      <Separator />

      <section className="flex flex-col gap-2">
        <h3 className="flex items-center gap-1.5 font-medium text-xs">
          <UsersIcon className="size-3.5" />
          参与 agent
        </h3>
        {members.length === 0 ? (
          <p className="text-muted-foreground text-xs">暂无成员</p>
        ) : (
          <ul className="flex flex-col gap-2">
            {members.map((member) => {
              const meta = presenceMeta(member.presence);
              return (
                <li key={member.agent_key} className="flex min-w-0 flex-col gap-0.5">
                  <div className="flex min-w-0 items-center gap-1.5">
                    <PresenceDot presence={member.presence} />
                    <span className="min-w-0 flex-1 truncate font-medium text-xs">
                      {memberDisplayName(member, agentByKey)}
                    </span>
                    <Badge variant="secondary" className="shrink-0 px-1 py-0 text-[9px]">
                      {ROLE_LABELS[member.role] ?? member.role}
                    </Badge>
                  </div>
                  <span className="pl-3.5 text-[11px] text-muted-foreground">
                    {meta.label}
                    {member.presence === "blocked" && member.blocked_on ? ` · 卡在 ${member.blocked_on}` : ""}
                    {member.presence === "blocked" && member.eta_minutes != null
                      ? ` · 预计还需 ${member.eta_minutes} 分钟`
                      : ""}
                  </span>
                </li>
              );
            })}
          </ul>
        )}
      </section>

      {quota && (
        <>
          <Separator />
          <section className="flex flex-col gap-1.5">
            <h3 className="font-medium text-xs">额度</h3>
            <p className="text-[11px] text-muted-foreground tabular-nums">
              预算 {fmtTokens(quota.budget_tokens)} · 已结算 {fmtTokens(quota.consumed_tokens)} · 剩余{" "}
              {fmtTokens(quota.remaining_tokens)}
            </p>
            <p className="text-[11px] text-muted-foreground">状态：{quota.state}</p>
          </section>
        </>
      )}
    </div>
  );
}
