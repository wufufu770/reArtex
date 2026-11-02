"use client";

import * as React from "react";

import { StatusBadge } from "@/components/status-badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import type { Agent, Conversation, TaskHallGroupDetail } from "@/lib/types";

import { fmtDateTime } from "../../_components/task-hall-format";

// memberStateBadge renders a member's live state, read off its own conversation.
function memberStateBadge(conv?: Conversation): React.ReactNode {
  if (!conv) return <span className="text-muted-foreground text-xs">会话不可见</span>;
  if (conv.running) return <StatusBadge domain="engine" value="exploring" dot />;
  return <StatusBadge domain="engine" value="idle" dot />;
}

// MembersTab 是成员列表与状态。
//
// 成员本身没有状态列（后端 chat_group_members 只有分工与加入时间），
// 运行状态从成员自己的会话上读：/api/conversations 会带每个会话的 running 标记。
export function MembersTab({
  detail,
  conversations,
  agents,
}: {
  detail: TaskHallGroupDetail;
  conversations: Conversation[];
  agents: Agent[];
}) {
  const convByID = React.useMemo(() => new Map(conversations.map((c) => [c.id, c])), [conversations]);
  const agentByKey = React.useMemo(() => new Map(agents.map((a) => [a.key, a])), [agents]);

  return (
    <Card>
      <CardHeader>
        <CardTitle>成员列表</CardTitle>
        <CardDescription>
          共 {detail.members.length} 位 · 分发者（auto）接待人并派单，成员各拿一个独立会话
        </CardDescription>
      </CardHeader>
      <CardContent>
        {detail.members.length === 0 ? (
          <Empty className="border border-dashed">
            <EmptyHeader>
              <EmptyTitle>暂无成员</EmptyTitle>
              <EmptyDescription>该大厅还没有登记成员。</EmptyDescription>
            </EmptyHeader>
          </Empty>
        ) : (
          <Table className="**:data-[slot='table-cell']:px-4 **:data-[slot='table-head']:px-4">
            <TableHeader className="[&_tr]:border-t">
              <TableRow>
                <TableHead>Agent</TableHead>
                <TableHead>分工</TableHead>
                <TableHead>会话</TableHead>
                <TableHead>状态</TableHead>
                <TableHead>加入时间</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {detail.members.map((m) => {
                const agent = agentByKey.get(m.agent_key);
                const conv = m.conversation_id ? convByID.get(m.conversation_id) : undefined;
                return (
                  <TableRow key={m.agent_key}>
                    <TableCell>
                      <div className="flex flex-col gap-0.5">
                        <span className="font-medium">{agent?.name ?? m.agent_key}</span>
                        <span className="font-mono text-muted-foreground text-xs">{m.agent_key}</span>
                      </div>
                    </TableCell>
                    <TableCell>
                      <StatusBadge domain="role" value={m.role} />
                    </TableCell>
                    <TableCell>
                      {m.conversation_id ? (
                        <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-muted-foreground text-xs">
                          #{m.conversation_id}
                        </code>
                      ) : (
                        <span className="text-muted-foreground text-xs">—</span>
                      )}
                    </TableCell>
                    <TableCell>{memberStateBadge(conv)}</TableCell>
                    <TableCell>
                      <span className="text-muted-foreground text-xs">{fmtDateTime(m.joined_at)}</span>
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}
