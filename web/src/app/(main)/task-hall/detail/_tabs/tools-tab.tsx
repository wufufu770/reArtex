"use client";

import * as React from "react";

import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Switch } from "@/components/ui/switch";
import { api } from "@/lib/api";
import type { TaskHallGroupDetail, Tool } from "@/lib/types";

// ToolsTab 是群内工具禁列表。
//
// 它是**禁列表**：出现的 key 就是被禁用的。表里没有「启用」列 ⇒ 群内配置只能让
// 权限更严，不可能放宽任何 agent 的工具集。所以这里的开关语义是「在本群禁用」。
export function ToolsTab({
  groupId,
  detail,
  onChanged,
}: {
  groupId: number;
  detail: TaskHallGroupDetail;
  onChanged: () => void;
}) {
  const [tools, setTools] = React.useState<Tool[]>([]);
  const [disabled, setDisabled] = React.useState<Record<string, boolean>>(detail.disabled_tools);
  const [busy, setBusy] = React.useState<string | null>(null);

  React.useEffect(() => {
    api
      .tools()
      .then(setTools)
      .catch(() => setTools([]));
  }, []);

  React.useEffect(() => {
    setDisabled(detail.disabled_tools);
  }, [detail.disabled_tools]);

  async function toggle(key: string, next: boolean) {
    setBusy(key);
    try {
      const result = next ? await api.disableTaskHallTool(groupId, key) : await api.enableTaskHallTool(groupId, key);
      setDisabled(result.disabled_tools ?? {});
      onChanged();
    } catch (e) {
      toast.error(`操作失败：${(e as Error).message}`);
    } finally {
      setBusy(null);
    }
  }

  const disabledKeys = Object.keys(disabled).filter((k) => disabled[k]);

  return (
    <Card>
      <CardHeader>
        <CardTitle>群内工具禁列表</CardTitle>
        <CardDescription>
          禁列表是收紧方向的：出现在这里的工具在本群不可用，群内配置无法放宽任何 agent 的工具集。 当前禁用{" "}
          {disabledKeys.length} 个。
        </CardDescription>
      </CardHeader>
      <CardContent>
        <ScrollArea className="h-[26rem]">
          <div className="flex flex-col gap-0.5 pr-3">
            {tools.length === 0 && <p className="py-6 text-center text-muted-foreground text-sm">没有可配置的工具</p>}
            {tools.map((t) => {
              const banned = disabled[t.key] === true;
              return (
                <div key={t.key} className="flex items-center gap-3 rounded-md px-2 py-1.5 hover:bg-accent">
                  <span className="font-mono text-sm">{t.key}</span>
                  <span className="min-w-0 flex-1 truncate text-muted-foreground text-xs">
                    {t.description || "（无描述）"}
                  </span>
                  {t.system && (
                    <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
                      内置
                    </Badge>
                  )}
                  {!t.enabled && (
                    <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
                      全局已停用
                    </Badge>
                  )}
                  <div className="flex items-center gap-2">
                    <span className="text-muted-foreground text-xs">{banned ? "本群禁用" : "本群可用"}</span>
                    <Switch
                      checked={banned}
                      disabled={busy === t.key}
                      onCheckedChange={(next) => void toggle(t.key, next)}
                      aria-label={`在本群禁用 ${t.key}`}
                    />
                  </div>
                </div>
              );
            })}
          </div>
        </ScrollArea>
      </CardContent>
    </Card>
  );
}
