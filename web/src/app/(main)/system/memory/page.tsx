"use client";

// 记忆治理页（0.13.3 前端对齐批：X4+X2 的宿主侧最小治理面）。
//
// 后端纪律（server/memory_api.go 为准）：host-only——agent 没有任何路径能把
// 条目转成 active；promote 先过三门评估（409 返回缺哪门的确定性清单，无
// --force）；promote/retire 全部写审计哈希链（source=memory-curation）。
// 本页只是把治理动作的表单挂上：操作人与理由必填（后端强制），动作结果如实。

import * as React from "react";

import { ArchiveRestoreIcon, ArchiveXIcon, RefreshCwIcon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { api } from "@/lib/api";
import type { MemoryEntry } from "@/lib/types";

const STATE_OPTIONS = [
  { value: "", label: "全部当前条目" },
  { value: "active", label: "active（已生效）" },
  { value: "quarantined", label: "quarantined（隔离待审）" },
] as const;

export default function MemoryPage() {
  const [entries, setEntries] = React.useState<MemoryEntry[]>([]);
  const [total, setTotal] = React.useState(0);
  const [state, setState] = React.useState<string>("");
  const [loading, setLoading] = React.useState(false);
  const [actor, setActor] = React.useState("");
  const [reason, setReason] = React.useState("");
  const [busyID, setBusyID] = React.useState<number | null>(null);

  const load = React.useCallback((s: string) => {
    setLoading(true);
    api
      .memoryEntries(s)
      .then((d) => {
        setEntries(d.entries ?? []);
        setTotal(d.total ?? 0);
      })
      .catch((e) => toast.error(`加载失败：${(e as Error).message}`))
      .finally(() => setLoading(false));
  }, []);

  React.useEffect(() => {
    load(state);
  }, [load, state]);

  const curate = async (entry: MemoryEntry, action: "promote" | "retire") => {
    if (!actor.trim() || !reason.trim()) {
      toast.error("操作人与理由必填（写入审计链）");
      return;
    }
    setBusyID(entry.ID);
    try {
      if (action === "promote") {
        await api.memoryPromote(entry.ID, actor.trim(), reason.trim());
        toast.success(`#${entry.ID} 已晋级（审计链已记）`);
      } else {
        await api.memoryRetire(entry.ID, actor.trim(), reason.trim());
        toast.success(`#${entry.ID} 已退役（审计链已记）`);
      }
      setReason("");
      load(state);
    } catch (e) {
      // 409 = 三门评估未过，响应体是缺哪门的确定性清单。
      toast.error(`${action === "promote" ? "晋级" : "退役"}被拒：${(e as Error).message}`);
    } finally {
      setBusyID(null);
    }
  };

  return (
    <div className="flex flex-col gap-4 p-4 md:p-6">
      <div className="flex flex-wrap items-center gap-2">
        <h1 className="text-lg font-semibold">记忆治理</h1>
        <Badge variant="outline">{total} 条</Badge>
        <div className="ml-auto flex items-center gap-2">
          <Select value={state} onValueChange={setState}>
            <SelectTrigger className="h-8 w-52 text-xs">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {STATE_OPTIONS.map((o) => (
                <SelectItem key={o.value} value={o.value}>
                  {o.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Button variant="outline" size="sm" className="h-8 text-xs" onClick={() => load(state)} disabled={loading}>
            <RefreshCwIcon className="size-3.5" />
            刷新
          </Button>
        </div>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">治理身份（写入审计链）</CardTitle>
        </CardHeader>
        <CardContent className="flex flex-wrap items-end gap-3">
          <Label className="text-muted-foreground text-xs">
            操作人
            <Input
              className="mt-1 h-8 w-40 text-sm"
              value={actor}
              onChange={(e) => setActor(e.target.value)}
              placeholder="操作者标识"
            />
          </Label>
          <Label className="text-muted-foreground text-xs">
            理由
            <Input
              className="mt-1 h-8 w-80 text-sm"
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              placeholder="本次治理的理由"
            />
          </Label>
        </CardContent>
      </Card>

      <div className="flex flex-col gap-2">
        {entries.length === 0 && !loading && (
          <div className="text-muted-foreground px-1 py-8 text-sm">暂无记忆条目</div>
        )}
        {entries.map((e) => (
          <div key={e.ID} className="flex flex-wrap items-center gap-3 rounded-lg border px-3 py-2">
            <Badge variant={e.State === "active" ? "default" : "secondary"} className="shrink-0 text-[10px]">
              {e.State}
            </Badge>
            <div className="min-w-0 flex-1">
              <div className="truncate text-sm font-medium">
                #{e.ID} {e.Title || e.TopicKey}
              </div>
              <div className="text-muted-foreground truncate text-xs">
                {e.Kind} · {e.ScopeType}#{e.ScopeID} · v{e.Version} · {e.SummaryLine}
              </div>
            </div>
            {e.State === "quarantined" ? (
              <Button
                size="sm"
                variant="outline"
                className="h-7 text-xs"
                disabled={busyID === e.ID}
                onClick={() => curate(e, "promote")}
              >
                <ArchiveRestoreIcon className="size-3.5" />
                晋级
              </Button>
            ) : (
              <Button
                size="sm"
                variant="outline"
                className="h-7 text-xs"
                disabled={busyID === e.ID}
                onClick={() => curate(e, "retire")}
              >
                <ArchiveXIcon className="size-3.5" />
                退役
              </Button>
            )}
          </div>
        ))}
      </div>
    </div>
  );
}
