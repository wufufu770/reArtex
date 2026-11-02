"use client";

import { Link2Icon } from "lucide-react";

import { StatusBadge } from "@/components/status-badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import type { StrategyEvidence } from "@/lib/types";

import { fmtDateTime, fmtDigest, REF_TYPE_LABEL } from "../../_components/strategy-format";

// EvidenceTab 展示策略的证据链。
//
// 证据链是策略的出处：没有它，一条策略就只是「凭空提权」的主张。
export function EvidenceTab({ evidence }: { evidence: StrategyEvidence[] }) {
  return (
    <div className="flex flex-col gap-4">
      <Card>
        <CardHeader>
          <CardTitle>证据链</CardTitle>
          <CardDescription>
            共 {evidence.length} 条 · 引用只存裸 id + 内容摘要，证据本体仍在漏洞 / 活动等表里
          </CardDescription>
        </CardHeader>
        <CardContent>
          {evidence.length === 0 ? (
            <Empty className="mx-4 border border-dashed lg:mx-6">
              <EmptyHeader>
                <EmptyTitle>还没有证据链</EmptyTitle>
                <EmptyDescription>
                  证据链是策略的出处 —— 一条没有出处的策略只是「凭空提权」的主张。 证据由策略被采纳时自动落链。
                </EmptyDescription>
              </EmptyHeader>
            </Empty>
          ) : (
            <Table className="**:data-[slot='table-cell']:px-4 **:data-[slot='table-head']:px-4">
              <TableHeader className="[&_tr]:border-t">
                <TableRow>
                  <TableHead>类型</TableHead>
                  <TableHead>引用</TableHead>
                  <TableHead>角色</TableHead>
                  <TableHead className="hidden md:table-cell">内容摘要</TableHead>
                  <TableHead>时间</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {evidence.map((e) => (
                  <TableRow key={`${e.ref_type}:${e.ref_id}:${e.role}:${e.content_digest}:${e.created_at}`}>
                    <TableCell>
                      <span className="text-sm">{REF_TYPE_LABEL[e.ref_type] ?? e.ref_type}</span>
                    </TableCell>
                    <TableCell>
                      <span className="font-mono text-xs">{e.ref_id}</span>
                    </TableCell>
                    <TableCell>
                      <StatusBadge domain="evidenceRole" value={e.role} />
                    </TableCell>
                    <TableCell className="hidden md:table-cell">
                      <span className="font-mono text-muted-foreground text-xs">{fmtDigest(e.content_digest)}</span>
                    </TableCell>
                    <TableCell>
                      <span className="text-muted-foreground text-xs">{fmtDateTime(e.created_at)}</span>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <p className="text-muted-foreground text-xs">
        <Link2Icon className="mr-1 inline size-3.5 align-text-bottom" />
        引用完整性由写入侧校验：库里的证据行没有级联外键，任务归档不会连带删掉它们。
      </p>
    </div>
  );
}
