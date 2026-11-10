"use client";

import * as React from "react";

import Link from "next/link";
import { useSearchParams } from "next/navigation";

import { ArrowLeftIcon, PinIcon } from "lucide-react";

import { StatusBadge } from "@/components/status-badge";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Separator } from "@/components/ui/separator";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api } from "@/lib/api";
import type { StrategyDetail } from "@/lib/types";

import { fmtScore } from "../_components/strategy-format";
import { BodyTab } from "./_tabs/body-tab";
import { DecisionTab } from "./_tabs/decision-tab";
import { EvidenceTab } from "./_tabs/evidence-tab";
import { OverviewTab } from "./_tabs/overview-tab";

// 策略库变动很慢（人工裁决 + curator 后台动作），15 秒一轮足够跟上。
const POLL_MS = 15_000;

const TABS = [
  { value: "overview", label: "总览" },
  { value: "body", label: "正文" },
  { value: "evidence", label: "证据链" },
  { value: "decision", label: "裁决" },
];

function StrategyDetailInner() {
  const searchParams = useSearchParams();
  const rawID = searchParams.get("id") ?? "";
  const id = Number(rawID);
  const [tab, setTab] = React.useState("overview");

  const [detail, setDetail] = React.useState<StrategyDetail | null>(null);
  const [loaded, setLoaded] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  const aliveRef = React.useRef(true);
  React.useEffect(() => {
    aliveRef.current = true;
    return () => {
      aliveRef.current = false;
    };
  }, []);

  const load = React.useCallback(async () => {
    if (!Number.isFinite(id) || id <= 0) return;
    try {
      const d = await api.strategy(id);
      if (!aliveRef.current) return;
      setDetail(d);
      setError(null);
    } catch (e) {
      if (!aliveRef.current) return;
      setError((e as Error).message);
    } finally {
      if (aliveRef.current) setLoaded(true);
    }
  }, [id]);

  React.useEffect(() => {
    void load();
    const t = setInterval(() => void load(), POLL_MS);
    return () => clearInterval(t);
  }, [load]);

  if (!Number.isFinite(id) || id <= 0) {
    return (
      <div className="flex flex-1 flex-col items-center justify-center gap-3 p-10 text-center">
        <p className="text-muted-foreground">缺少策略 id</p>
        <Button asChild variant="outline">
          <Link href="/strategies">
            <ArrowLeftIcon /> 返回策略库
          </Link>
        </Button>
      </div>
    );
  }

  if (!detail) {
    return (
      <div className="flex flex-1 flex-col items-center justify-center gap-3 p-10 text-center">
        <p className="text-muted-foreground">{loaded ? (error ?? "策略不存在或已被删除") : "加载中…"}</p>
        {loaded && (
          <Button asChild variant="outline">
            <Link href="/strategies">
              <ArrowLeftIcon /> 返回策略库
            </Link>
          </Button>
        )}
      </div>
    );
  }

  const s = detail.strategy;

  return (
    <Tabs value={tab} onValueChange={setTab} className="flex flex-1 flex-col gap-4">
      <div className="flex flex-col gap-2">
        <div className="flex items-center gap-2">
          <Button asChild variant="ghost" size="icon" className="size-7">
            <Link href="/strategies">
              <ArrowLeftIcon />
            </Link>
          </Button>
          <h1 className="min-w-0 flex-1 truncate font-semibold text-xl tracking-tight">{s.name}</h1>
          <code className="hidden rounded bg-muted px-1.5 py-0.5 font-mono text-muted-foreground text-xs sm:inline">
            #{s.id}
          </code>
          <StatusBadge domain="strategy" value={s.state} dot />
          {s.pinned && (
            <Badge variant="outline" className="gap-1 px-1.5 py-0 text-[10px]">
              <PinIcon className="size-3" />
              唯一证人
            </Badge>
          )}
        </div>
        <div className="flex flex-wrap items-center gap-2 pl-9 text-muted-foreground text-xs">
          <span>版本 v{s.version}</span>
          <Separator orientation="vertical" className="h-3" />
          <span>效用 {fmtScore(s.utility)}</span>
          <Separator orientation="vertical" className="h-3" />
          <span>置信 {fmtScore(s.confidence)}</span>
          <Separator orientation="vertical" className="h-3" />
          <span>
            用 {s.usage_count} · 命中 {s.hit_count} · 未命中 {s.miss_count}
          </span>
          <Separator orientation="vertical" className="h-3" />
          <span>证据 {detail.evidence.length} 条</span>
        </div>
      </div>

      <TabsList variant="default" className="min-w-max">
        {TABS.map((t) => (
          <TabsTrigger key={t.value} value={t.value}>
            {t.label}
          </TabsTrigger>
        ))}
      </TabsList>

      <TabsContent value="overview" className="mt-0">
        <OverviewTab detail={detail} />
      </TabsContent>
      <TabsContent value="body" className="mt-0">
        <BodyTab strategy={s} onSaved={() => void load()} />
      </TabsContent>
      <TabsContent value="evidence" className="mt-0">
        <EvidenceTab evidence={detail.evidence} />
      </TabsContent>
      <TabsContent value="decision" className="mt-0">
        <DecisionTab strategy={s} onChanged={() => void load()} />
      </TabsContent>
    </Tabs>
  );
}

// useSearchParams must sit under a Suspense boundary for static export.
export default function StrategyDetailPage() {
  return (
    <React.Suspense fallback={null}>
      <StrategyDetailInner />
    </React.Suspense>
  );
}
