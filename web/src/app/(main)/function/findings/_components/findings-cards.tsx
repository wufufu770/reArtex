"use client";

import * as React from "react";

import Link from "next/link";

import { FlaskConicalIcon, RotateCcwIcon } from "lucide-react";

import { StatusBadge } from "@/components/status-badge";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import type { ActiveFindingRetest, Finding } from "@/lib/types";

import { findingRowKey, fmtTime } from "./findings-table";

interface FindingsCardsProps {
  items: Finding[];
  /** 复测进行中的映射：finding_id → 复测记录（有值时按钮换成「复测中」会话链接）。 */
  activeRetests: Record<string, ActiveFindingRetest>;
  onRetest: (finding: Finding) => void;
  onDeepen: (finding: Finding) => void;
}

// FindingsCards 是发现列表的卡片形态：自适应多列网格、页面自然【上下滚动】
//（用户拍板：要上下滑，不要左右滑）。与 FindingsTable 消费同一页数据、同一批
// 回调；分页与筛选仍在外层页面。
export function FindingsCards({ items, activeRetests, onRetest, onDeepen }: FindingsCardsProps) {
  if (items.length === 0) {
    return <p className="py-12 text-center text-muted-foreground text-sm">没有匹配的发现。</p>;
  }

  return (
    <div className="relative px-4 py-3">
      {/* 用户拍板：上下滑动（非左右）——自适应多列网格，页面自然纵向滚动 */}
      <div className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-4">
        {items.map((f) => {
          const retest = f.finding_id ? activeRetests[f.finding_id] : undefined;
          return (
            <article key={findingRowKey(f)} className="flex flex-col gap-2 rounded-lg border bg-background p-3">
              <div className="flex flex-wrap items-center gap-1.5">
                <StatusBadge domain="severity" value={f.severity} dot />
                <StatusBadge domain="finding" value={f.status} dot />
                <Badge variant="outline">流量证据 {f.traffic_count ?? 0} 条</Badge>
              </div>
              <div className="flex min-w-0 flex-col gap-1">
                {f.finding_id ? (
                  <Link
                    href={`/function/findings/detail?id=${f.finding_id}`}
                    className="truncate font-semibold hover:text-primary hover:underline"
                    title="查看发现详情"
                  >
                    {f.name || f.vulnclass || "未分类"}
                  </Link>
                ) : (
                  <span className="truncate font-semibold">{f.name || f.vulnclass || "未分类"}</span>
                )}
                {f.summary && <span className="line-clamp-2 text-muted-foreground text-xs">{f.summary}</span>}
              </div>
              <div className="flex min-w-0 items-center gap-2">
                {f.assets && f.assets.length > 0 ? (
                  <>
                    <code
                      className="min-w-0 truncate rounded bg-muted px-1.5 py-0.5 font-mono text-xs"
                      title={`${f.assets[0].type} · ${f.assets[0].label}`}
                    >
                      {f.assets[0].label}
                    </code>
                    {f.assets.length > 1 && (
                      <span className="shrink-0 text-muted-foreground text-xs">+{f.assets.length - 1}</span>
                    )}
                  </>
                ) : (
                  <span className="text-muted-foreground text-xs">资产 —</span>
                )}
                <span className="ml-auto shrink-0 text-muted-foreground text-xs tabular-nums">{fmtTime(f.ts)}</span>
              </div>
              <div className="mt-auto flex items-center gap-1 border-t pt-2">
                {retest ? (
                  <Button asChild size="sm" variant="ghost">
                    <Link href={`/chat?c=${retest.conversation_id}`} title="查看正在进行的复测会话">
                      <Spinner data-icon="inline-start" />
                      复测中
                    </Link>
                  </Button>
                ) : null}
                {!retest && f.finding_id && !f.inherited ? (
                  <Button size="sm" variant="ghost" onClick={() => onRetest(f)} title="在独立会话中复测该漏洞">
                    <RotateCcwIcon data-icon="inline-start" />
                    复测
                  </Button>
                ) : null}
                {f.finding_id && f.task_id && (
                  <Button size="sm" variant="ghost" onClick={() => onDeepen(f)}>
                    <FlaskConicalIcon data-icon="inline-start" />
                    深入
                  </Button>
                )}
              </div>
            </article>
          );
        })}
      </div>
    </div>
  );
}
