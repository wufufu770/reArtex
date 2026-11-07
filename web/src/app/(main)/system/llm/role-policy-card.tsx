"use client";

// P2 分层经济：每角色模型策略编辑卡（settings `llm.role_policy`）。
// 决策槽位（planner/mainagent）结构性禁止 backup——后端写入闸校验，前端同步提示；
// worker 可带 backup（额度耗尽自动轮换）。候选 profile 须已配 key 且非 pool_exclude。

import * as React from "react";

import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import type { LLMProfile } from "@/lib/types";

type Lane = "planner" | "worker" | "mainagent" | "normalizer";

const LANES: { key: Lane; label: string; hint: string; decision: boolean }[] = [
  { key: "planner", label: "planner（规划）", hint: "决策槽位：额度耗尽=暂停+通知，不自动降级", decision: true },
  { key: "worker", label: "worker（执行）", hint: "吞吐槽位：额度耗尽自动轮换到 backup", decision: false },
  { key: "mainagent", label: "mainagent（复核/验证）", hint: "决策槽位：异层模型，不自动降级", decision: true },
  {
    key: "normalizer",
    label: "normalizer（SoD 规整器）",
    hint: "基础设施槽位：独立模型档，规整调用不再与任务链抢限流（B3）",
    decision: false,
  },
];

export function RolePolicyCard({ profiles, onSaved }: { profiles: LLMProfile[]; onSaved: () => void }) {
  const [_raw, setRaw] = React.useState("");
  const [loaded, setLoaded] = React.useState(false);
  const [saving, setSaving] = React.useState(false);
  const [lanes, setLanes] = React.useState<Record<Lane, string>>({
    planner: "",
    worker: "",
    mainagent: "",
    normalizer: "",
  });
  const [workerBackup, setWorkerBackup] = React.useState("");

  React.useEffect(() => {
    api
      .rolePolicy()
      .then((rawJson) => {
        setRaw(rawJson);
        try {
          const parsed = JSON.parse(rawJson || "{}") as Record<Lane, { primary?: number; backup?: number }>;
          setLanes({
            planner: parsed.planner?.primary ? String(parsed.planner.primary) : "",
            worker: parsed.worker?.primary ? String(parsed.worker.primary) : "",
            mainagent: parsed.mainagent?.primary ? String(parsed.mainagent.primary) : "",
            normalizer: parsed.normalizer?.primary ? String(parsed.normalizer.primary) : "",
          });
          setWorkerBackup(parsed.worker?.backup ? String(parsed.worker.backup) : "");
        } catch {
          /* 坏配置回显原样，保存时后端会拒 */
        }
        setLoaded(true);
      })
      .catch((e) => toast.error(`读取角色策略失败：${(e as Error).message}`));
  }, []);

  async function save() {
    if (saving) return;
    // 组装 JSON；空 lane 不写入（未配置=回退任务链/全局）。
    const policy: Record<string, { primary: number; backup?: number }> = {};
    const entries: [Lane, string, string | undefined][] = [
      ["planner", lanes.planner, undefined],
      ["worker", lanes.worker, workerBackup],
      ["mainagent", lanes.mainagent, undefined],
      ["normalizer", lanes.normalizer, undefined],
    ];
    for (const [lane, primary, backup] of entries) {
      const p = Number(primary.trim());
      if (!Number.isFinite(p) || p <= 0) continue;
      policy[lane] = { primary: p };
      const b = Number((backup ?? "").trim());
      if (Number.isFinite(b) && b > 0 && !(lane === "worker" && b === p)) policy[lane].backup = b;
    }
    setSaving(true);
    try {
      await api.setRolePolicy(JSON.stringify(policy));
      toast.success("角色策略已保存：无显式绑定的 lane 即刻生效");
      onSaved();
    } catch (e) {
      toast.error(`保存失败：${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  const usable = profiles.filter((p) => !p.pool_exclude);
  if (!loaded) return null;

  return (
    <Card>
      <CardHeader>
        <CardTitle>角色模型策略（分层经济）</CardTitle>
        <CardDescription>
          为「无显式绑定」的引擎角色指定模型档：planner/mainagent 只配主档（额度耗尽暂停+通知）， worker
          可配备用档（额度耗尽自动轮换）。已显式绑定模型的 agent 不受此处影响。
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4">
        {LANES.map((lane) => (
          <div key={lane.key} className="grid gap-1.5">
            <Label htmlFor={`rp-${lane.key}`}>{lane.label}</Label>
            <Input
              id={`rp-${lane.key}`}
              inputMode="numeric"
              placeholder={`profile id（留空=跟随任务链/全局）`}
              value={lanes[lane.key]}
              onChange={(e) => setLanes((prev) => ({ ...prev, [lane.key]: e.target.value }))}
            />
            <p className="text-muted-foreground text-xs">{lane.hint}</p>
            {lane.key === "worker" && (
              <>
                <Label htmlFor="rp-worker-backup" className="mt-1">
                  worker 备用档（可选）
                </Label>
                <Input
                  id="rp-worker-backup"
                  inputMode="numeric"
                  placeholder="primary 额度耗尽后自动轮换到它"
                  value={workerBackup}
                  onChange={(e) => setWorkerBackup(e.target.value)}
                />
              </>
            )}
          </div>
        ))}
        <p className="text-muted-foreground text-xs">
          可用 profile：{usable.map((p) => `${p.id}=${p.name}`).join(" · ") || "无"}
        </p>
        <div className="flex justify-end">
          <Button onClick={() => void save()} disabled={saving}>
            保存角色策略
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}
