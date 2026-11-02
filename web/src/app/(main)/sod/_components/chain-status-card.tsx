"use client";

import * as React from "react";

import Link from "next/link";

import { CircleDashedIcon, Link2Icon, ShieldAlertIcon, ShieldCheckIcon } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { api } from "@/lib/api";
import { type Tone, toneClasses } from "@/lib/status";
import type { AuditChainVerification } from "@/lib/types";
import { cn } from "@/lib/utils";

// ChainStatusCard 显示审计链校验的结果。
//
// 为什么单独一个组件：账本页与链校验页都要看到「链还完整吗」，两处各写一遍
// 会出现两份口径 —— 而这条链的结论直接决定账本能不能当证据用。
//
// ⚠️ 断链不是接口故障：后端回 200 + intact=false，那是**结论**（链被改过）。
// 所以这里把它渲染成一个状态，而不是把请求当异常抛掉。
export function ChainStatusCard({ compact = false }: { compact?: boolean }) {
  const [result, setResult] = React.useState<AuditChainVerification | null>(null);
  const [failed, setFailed] = React.useState(false);
  const [loading, setLoading] = React.useState(true);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      setResult(await api.auditChain());
      setFailed(false);
    } catch {
      setResult(null);
      setFailed(true);
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  if (loading && !result) {
    return (
      <div className="flex items-center gap-2 rounded-xl border border-border bg-card px-4 py-3 text-muted-foreground text-xs">
        <Spinner className="size-3.5" />
        正在校验审计链…
      </div>
    );
  }

  // 校验没跑成 = 未知，不是「完整」。
  if (failed || !result) {
    return (
      <div className="flex flex-col gap-2 rounded-xl border border-border bg-card px-4 py-3">
        <div className="flex flex-wrap items-center gap-2">
          <CircleDashedIcon className="size-4 text-muted-foreground" />
          <span className="font-semibold text-sm">审计链状态未知</span>
          <Badge variant="outline" className={cn("text-[10px]", toneClasses.slate)}>
            校验未完成
          </Badge>
        </div>
        <p className="text-muted-foreground text-xs">
          没能读到审计链校验接口 —— 这是「不知道链还完整吗」，不是「链是完整的」。
        </p>
        <Button type="button" variant="outline" size="sm" className="w-fit" onClick={() => void load()}>
          重新校验
        </Button>
      </div>
    );
  }

  const tone: Tone = result.intact ? "green" : "red";
  const Icon = result.intact ? ShieldCheckIcon : ShieldAlertIcon;

  if (compact) {
    return (
      <div className="flex flex-wrap items-center gap-2 rounded-xl border border-border bg-card px-4 py-2.5">
        <Icon className="size-4 text-muted-foreground" />
        <span className="font-medium text-sm">审计链</span>
        <Badge variant="outline" className={cn("text-[10px]", toneClasses[tone])}>
          {result.intact ? "完整" : `已断裂 · 第 ${result.broken_at_seq} 条`}
        </Badge>
        <Button asChild variant="ghost" size="sm" className="ml-auto">
          <Link href="/sod/chain">
            <Link2Icon /> 查看校验
          </Link>
        </Button>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-2 rounded-xl border border-border bg-card px-4 py-3">
      <div className="flex flex-wrap items-center gap-2">
        <Icon className="size-4 text-muted-foreground" />
        <span className="font-semibold text-sm">审计链校验</span>
        <Badge variant="outline" className={cn("text-[10px]", toneClasses[tone])}>
          {result.intact ? "链完整" : "链已断裂"}
        </Badge>
      </div>

      {result.intact ? (
        <p className="text-muted-foreground text-xs">
          每条审计记录的摘要都依赖上一条 ⇒ 链完整说明这批记录没被改过，账本里的裁决可以当证据用。
        </p>
      ) : (
        <div className="flex flex-col gap-1.5">
          <p className="text-muted-foreground text-xs">
            第一处断裂在第 <span className="font-mono text-foreground">{result.broken_at_seq}</span> 条 ——
            从这里往后的记录摘要都对不上了。
          </p>
          {result.error && (
            <pre className="max-h-32 overflow-auto whitespace-pre-wrap rounded-md bg-muted/50 p-2 font-mono text-[10px]">
              {result.error}
            </pre>
          )}
        </div>
      )}

      <Button type="button" variant="outline" size="sm" className="w-fit" onClick={() => void load()}>
        {loading ? "重新校验中…" : "重新校验"}
      </Button>
    </div>
  );
}
