"use client";

import * as React from "react";

import Link from "next/link";

import { ChevronLeftIcon, ChevronRightIcon, FlaskConicalIcon, RotateCcwIcon } from "lucide-react";

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

// FindingsCards 是发现列表的卡片形态：横向滑动卡片流。与 FindingsTable 消费
// 同一页数据、同一批回调，只是把行渲染换成卡片；分页与筛选仍在外层页面。
// 滚动不用 embla 之类的轮播依赖：原生 snap + 自绘左右滚动按钮（scrollTo 平滑滚）。
export function FindingsCards({ items, activeRetests, onRetest, onDeepen }: FindingsCardsProps) {
  const scrollerRef = React.useRef<HTMLDivElement>(null);
  const [canScrollLeft, setCanScrollLeft] = React.useState(false);
  const [canScrollRight, setCanScrollRight] = React.useState(false);

  const updateArrows = React.useCallback(() => {
    const el = scrollerRef.current;
    if (!el) return;
    setCanScrollLeft(el.scrollLeft > 0);
    setCanScrollRight(el.scrollLeft + el.clientWidth < el.scrollWidth - 1);
  }, []);

  // 数据换页后 DOM 已重渲染,重新量一次两端可滚性(与 onScroll 各自触发);
  // 空列表直接把按钮收起来。
  React.useEffect(() => {
    if (items.length === 0) {
      setCanScrollLeft(false);
      setCanScrollRight(false);
      return;
    }
    const el = scrollerRef.current;
    if (!el) return;
    setCanScrollLeft(el.scrollLeft > 0);
    setCanScrollRight(el.scrollLeft + el.clientWidth < el.scrollWidth - 1);
  }, [items]);

  function scrollCards(direction: -1 | 1) {
    const el = scrollerRef.current;
    if (!el) return;
    el.scrollTo({ left: el.scrollLeft + direction * el.clientWidth * 0.8, behavior: "smooth" });
  }

  if (items.length === 0) {
    return <p className="py-12 text-center text-muted-foreground text-sm">没有匹配的发现。</p>;
  }

  return (
    <div className="relative">
      <div ref={scrollerRef} className="flex snap-x gap-3 overflow-x-auto px-4 py-3" onScroll={updateArrows}>
        {items.map((f) => {
          const retest = f.finding_id ? activeRetests[f.finding_id] : undefined;
          return (
            <article
              key={findingRowKey(f)}
              className="flex w-[320px] min-w-[320px] shrink-0 snap-start flex-col gap-2 rounded-lg border bg-background p-3"
            >
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
      {canScrollLeft && (
        <Button
          variant="outline"
          size="icon"
          className="absolute top-1/2 left-2 z-10 size-7 -translate-y-1/2 rounded-full"
          onClick={() => scrollCards(-1)}
          aria-label="向左滚动"
        >
          <ChevronLeftIcon />
        </Button>
      )}
      {canScrollRight && (
        <Button
          variant="outline"
          size="icon"
          className="absolute top-1/2 right-2 z-10 size-7 -translate-y-1/2 rounded-full"
          onClick={() => scrollCards(1)}
          aria-label="向右滚动"
        >
          <ChevronRightIcon />
        </Button>
      )}
    </div>
  );
}
