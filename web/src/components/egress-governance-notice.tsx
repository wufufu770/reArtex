"use client";

import * as React from "react";

import { CircleDashedIcon, ShieldAlertIcon, ShieldCheckIcon } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { api } from "@/lib/api";
import { type EgressGovernanceView, egressGovernanceView } from "@/lib/egress-governance";
import { toneClasses } from "@/lib/status";
import { cn } from "@/lib/utils";

const PANEL_TONE: Record<EgressGovernanceView["status"], string> = {
  incomplete: "bg-amber-500/5 ring-amber-500/30",
  unknown: "bg-muted/30 ring-foreground/10",
  complete: "bg-emerald-500/5 ring-emerald-500/25",
};

const TITLE_TONE: Record<EgressGovernanceView["status"], string> = {
  incomplete: "text-amber-600 dark:text-amber-400",
  unknown: "text-slate-600 dark:text-slate-400",
  complete: "text-emerald-600 dark:text-emerald-400",
};

const STATUS_ICON: Record<EgressGovernanceView["status"], React.ElementType> = {
  incomplete: ShieldAlertIcon,
  unknown: CircleDashedIcon,
  complete: ShieldCheckIcon,
};

export function EgressGovernanceNotice({ className }: { className?: string }) {
  const [view, setView] = React.useState<EgressGovernanceView | null>(null);
  const [loading, setLoading] = React.useState(true);
  const mounted = React.useRef(true);

  React.useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  const load = React.useCallback(() => {
    setLoading(true);
    api
      .egressGovernance()
      .then((state) => {
        if (mounted.current) setView(egressGovernanceView(state));
      })
      .catch(() => {
        if (mounted.current) setView(egressGovernanceView(null));
      })
      .finally(() => {
        if (mounted.current) setLoading(false);
      });
  }, []);

  React.useEffect(() => {
    load();
  }, [load]);

  if (!view) {
    return (
      <div className={cn("rounded-xl bg-card px-4 py-3 ring-1 ring-foreground/10", className)}>
        <div className="flex items-center gap-2 text-xs text-muted-foreground">
          <Spinner className="size-3.5" />
          正在读取出网治理状态…
        </div>
      </div>
    );
  }

  const Icon = STATUS_ICON[view.status];

  return (
    <div className={cn("rounded-xl px-4 py-3 ring-1", PANEL_TONE[view.status], className)}>
      <div className="flex items-start gap-2.5">
        <Icon className={cn("mt-0.5 size-4 shrink-0", TITLE_TONE[view.status])} />
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <span className={cn("text-sm font-semibold", TITLE_TONE[view.status])}>{view.label}</span>
            <Badge variant="outline" className={cn("text-[10px]", toneClasses[view.tone])}>
              {view.badge}
            </Badge>
          </div>
          <p className="mt-1 text-xs text-muted-foreground">{view.summary}</p>
          {view.channels.length > 0 && (
            <ul className="mt-2 flex flex-col gap-1">
              {view.channels.map((channel) => (
                <li key={channel} className="flex gap-1.5 text-xs text-muted-foreground">
                  <span className="mt-1.5 size-1 shrink-0 rounded-full bg-current opacity-50" />
                  <span className="min-w-0">{channel}</span>
                </li>
              ))}
            </ul>
          )}
          {view.status === "unknown" && (
            <Button type="button" variant="outline" size="sm" className="mt-2" onClick={load} disabled={loading}>
              {loading ? "重新读取中…" : "重新读取"}
            </Button>
          )}
        </div>
      </div>
    </div>
  );
}
