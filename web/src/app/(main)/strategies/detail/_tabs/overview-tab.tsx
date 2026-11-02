"use client";

import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import type { StrategyDetail } from "@/lib/types";

import { fmtDateTime, fmtScore, fmtSigned } from "../../_components/strategy-format";

// OverviewTab 是策略的身份与读数。
export function OverviewTab({ detail }: { detail: StrategyDetail }) {
  const s = detail.strategy;

  const metrics = [
    { label: "置信 confidence", value: fmtScore(s.confidence) },
    { label: "效用 utility（缓存列）", value: fmtScore(s.utility) },
    { label: "运行效用 run_utility", value: fmtSigned(s.run_utility) },
    { label: "使用次数 usage", value: String(s.usage_count) },
    { label: "命中 hit", value: String(s.hit_count) },
    { label: "未命中 miss", value: String(s.miss_count) },
    { label: "检索次数 retrieval", value: String(s.retrieval_count) },
    { label: "版本 version", value: `v${s.version}` },
  ];

  const timeline = [
    { label: "生效时间 valid_from", value: fmtDateTime(s.valid_from) },
    { label: "失效时间 invalid_at", value: fmtDateTime(s.invalid_at) },
    { label: "硬 TTL expired_at", value: fmtDateTime(s.expired_at) },
    { label: "退役时间 retired_at", value: fmtDateTime(s.retired_at) },
    { label: "创建时间 created_at", value: fmtDateTime(s.created_at) },
    { label: "更新时间 updated_at", value: fmtDateTime(s.updated_at) },
  ];

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <CardHeader>
          <CardTitle>摘要</CardTitle>
          <CardDescription>这条策略讲的是「遇到什么目标、按什么步骤打」</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          <p className="text-sm">{s.summary || "（这条策略没有写摘要）"}</p>
          {s.state === "retired" && s.retired_reason && (
            <div className="rounded-md border border-destructive/40 bg-destructive/5 p-3">
              <p className="font-medium text-sm">退役理由</p>
              <p className="text-muted-foreground text-sm">{s.retired_reason}</p>
              <p className="text-muted-foreground text-xs">退役 ≠ 删除：策略行与证据链都还在，可以被审计与追溯。</p>
            </div>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>读数</CardTitle>
          <CardDescription>效用与置信来自证据链的统计，运行效用是 +1 / -1 的累加器</CardDescription>
        </CardHeader>
        <CardContent>
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
            {metrics.map((m) => (
              <div key={m.label} className="flex flex-col gap-0.5">
                <span className="text-muted-foreground text-xs">{m.label}</span>
                <span className="font-medium tabular-nums">{m.value}</span>
              </div>
            ))}
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>时间</CardTitle>
          <CardDescription>双时态：冲突不删数据，而是失效 + 叙述化改写</CardDescription>
        </CardHeader>
        <CardContent>
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            {timeline.map((t) => (
              <div key={t.label} className="flex flex-col gap-0.5">
                <span className="text-muted-foreground text-xs">{t.label}</span>
                <span className="font-medium text-sm tabular-nums">{t.value}</span>
              </div>
            ))}
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
