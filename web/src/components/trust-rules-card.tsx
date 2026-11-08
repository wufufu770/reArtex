"use client";

// F2 信任规则卡（方案 v3）：allow 通路上的具名视图。
// 保存前先「命中回放预览」看 7 天会放行多少真实调用；^$ 锚定 + Tier3 拒绝 +
// 默认停用（confirm 才启用）由后端强制，这里照实展示错误。

import * as React from "react";

import { ShieldCheckIcon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { api } from "@/lib/api";
import type { InterceptRule, Tool } from "@/lib/types";

interface Preview {
  would_allow: number;
  scanned: number;
  window_days: number;
  samples: { id: number; preview: string }[];
}

export function TrustRulesCard({ tools }: { tools: Tool[] }) {
  const [rules, setRules] = React.useState<InterceptRule[]>([]);
  const [max, _setMax] = React.useState(50);
  const [tool, setTool] = React.useState("");
  const [pattern, setPattern] = React.useState("");
  const [reason, setReason] = React.useState("");
  const [confirm, setConfirm] = React.useState(false);
  const [preview, setPreview] = React.useState<Preview | null>(null);
  const [saving, setSaving] = React.useState(false);

  const load = React.useCallback(async () => {
    try {
      const [rules, meta] = await Promise.all([api.trustRules(), api.interceptRules()]);
      setRules(rules);
      void meta;
    } catch (e) {
      toast.error(`读取信任规则失败：${(e as Error).message}`);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  async function runPreview() {
    if (!tool || !pattern.trim()) {
      toast.error("先填工具与 pattern");
      return;
    }
    try {
      setPreview(await api.previewTrustRule({ tool, pattern: pattern.trim() }));
    } catch (e) {
      toast.error(`预览失败：${(e as Error).message}`);
    }
  }

  async function save() {
    if (!tool || !pattern.trim()) {
      toast.error("工具与 pattern 必填");
      return;
    }
    setSaving(true);
    try {
      const r = await api.createTrustRule({
        tool,
        pattern: pattern.trim(),
        reason: reason.trim(),
        operator: "sir",
        confirm,
      });
      toast.success(confirm ? "信任规则已创建并启用" : "信任规则已创建（停用中，确认后启用）");
      void r;
      setTool("");
      setPattern("");
      setReason("");
      setConfirm(false);
      setPreview(null);
      await load();
    } catch (e) {
      toast.error(`创建失败：${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  const candidateTools = tools.filter((t) => t.enabled !== false).map((t) => t.key);

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <ShieldCheckIcon className="size-4" /> 信任规则
        </CardTitle>
        <CardDescription>
          命中即放行（先于职责分离链）。pattern 必须 ^ 开头 $ 结尾；Tier3 高危工具不可信任化；默认停用，确认后启用；上限{" "}
          {max} 条。
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-3">
        <div className="flex flex-wrap items-end gap-2">
          <div className="grid gap-1.5">
            <Label htmlFor="trust-tool">工具</Label>
            <Select value={tool} onValueChange={setTool}>
              <SelectTrigger id="trust-tool" className="w-44">
                <SelectValue placeholder="选择工具" />
              </SelectTrigger>
              <SelectContent>
                {candidateTools.map((key) => (
                  <SelectItem key={key} value={key}>
                    {key}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="grid min-w-64 flex-1 gap-1.5">
            <Label htmlFor="trust-pattern">pattern（^…$ 锚定）</Label>
            <Input
              id="trust-pattern"
              value={pattern}
              onChange={(e) => setPattern(e.target.value)}
              placeholder={"例：^grep .* /app/data/tasks/\\d+.*$"}
              className="font-mono"
            />
          </div>
          <div className="grid min-w-52 flex-1 gap-1.5">
            <Label htmlFor="trust-reason">理由（审计留痕）</Label>
            <Input
              id="trust-reason"
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              placeholder="为什么值得信任"
            />
          </div>
          <div className="flex items-center gap-2 pb-1">
            <Switch id="trust-confirm" checked={confirm} onCheckedChange={setConfirm} />
            <Label htmlFor="trust-confirm" className="text-xs">
              确认启用
            </Label>
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Button size="sm" variant="outline" onClick={() => void runPreview()}>
            回放预览（近 7 天）
          </Button>
          <Button size="sm" onClick={() => void save()} disabled={saving}>
            保存信任规则
          </Button>
          {preview && (
            <Badge variant={preview.would_allow === 0 ? "secondary" : "outline"} className="text-xs">
              预览：7 天内会放行 {preview.would_allow} / 扫描 {preview.scanned} 条真实调用
            </Badge>
          )}
        </div>
        {preview && preview.samples.length > 0 && (
          <div className="rounded-md border p-2">
            <p className="mb-1 text-muted-foreground text-xs">将被放行的调用样例：</p>
            {preview.samples.map((s) => (
              <p key={s.id} className="truncate font-mono text-[11px] text-muted-foreground">
                #{s.id} {s.preview}
              </p>
            ))}
          </div>
        )}
        <div className="grid gap-1.5">
          <p className="font-medium text-sm">
            现有信任规则（{rules.length}/{max}）
          </p>
          {rules.length === 0 ? (
            <p className="text-muted-foreground text-xs">
              还没有信任规则。高频被人工批准/反复误拒的动作适合沉淀到这里。
            </p>
          ) : (
            rules.map((rule) => (
              <div key={rule.id} className="flex flex-wrap items-center gap-2 rounded-md border p-2">
                <Badge variant={rule.enabled ? "default" : "secondary"} className="text-[10px]">
                  {rule.enabled ? "启用" : "停用"}
                </Badge>
                <span className="min-w-0 flex-1 truncate font-mono text-xs">{rule.pattern}</span>
                <span className="text-[10px] text-muted-foreground">{rule.message}</span>
              </div>
            ))
          )}
        </div>
      </CardContent>
    </Card>
  );
}
