"use client";

import * as React from "react";

import { ArchiveIcon, PinIcon, PinOffIcon, ShieldCheckIcon } from "lucide-react";
import { toast } from "sonner";

import { StatusBadge } from "@/components/status-badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { api } from "@/lib/api";
import type { Strategy } from "@/lib/types";

import { DecisionDialog } from "../../_components/decision-dialog";

// 可退役的状态：后端只允许从隔离态 / 在役态退役，其它状态回 409。
const RETIRABLE = ["quarantined", "active"];

// DecisionTab 是这条策略的人工裁决面。
//
// 三个写动作都是「裁决写」：理由与操作人必填（后端缺一个就回 400），
// 并且这一笔会进 audit_events 哈希链 —— 事后能查「谁做的、为什么」。
export function DecisionTab({ strategy, onChanged }: { strategy: Strategy; onChanged: () => void }) {
  const [pinTarget, setPinTarget] = React.useState<boolean | null>(null);
  const [retireOpen, setRetireOpen] = React.useState(false);

  const retirable = RETIRABLE.includes(strategy.state);

  async function confirmPin(reason: string, actor: string) {
    if (pinTarget === null) return;
    const result = await api.setStrategyPinned(strategy.id, { pinned: pinTarget, reason, actor });
    if (result.note) {
      toast.warning(result.note, { duration: 12000 });
      return;
    }
    toast.success(pinTarget ? "已置位 pin" : "已解除 pin");
  }

  async function confirmRetire(reason: string, actor: string) {
    await api.retireStrategy(strategy.id, { reason, actor });
    toast.success("策略已退役");
  }

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <CardHeader>
          <CardTitle>pin（唯一证人保护）</CardTitle>
          <CardDescription>置位与解除都是人工裁决：必须写明理由与操作人，并进审计哈希链</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          <div className="flex flex-wrap items-center gap-2">
            <StatusBadge domain="strategy" value={strategy.state} dot />
            {strategy.pinned ? (
              <span className="flex items-center gap-1.5 text-sm">
                <PinIcon className="size-4" />
                当前已 pin
              </span>
            ) : (
              <span className="text-muted-foreground text-sm">当前未 pin</span>
            )}
          </div>
          <p className="text-muted-foreground text-sm">
            被 pin 的策略往往是通往某个 confirmed 漏洞的唯一已知路径 —— 解 pin 会让那份漏洞报告再也复现不出来， 所以解
            pin 一定要有人明确决定并留下理由。
          </p>
          <div>
            {strategy.pinned ? (
              <Button variant="outline" onClick={() => setPinTarget(false)}>
                <PinOffIcon /> 解除 pin
              </Button>
            ) : (
              <Button onClick={() => setPinTarget(true)}>
                <PinIcon /> 置位 pin
              </Button>
            )}
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>退役</CardTitle>
          <CardDescription>退役 ≠ 删除：写 state=retired + 退役理由，行与证据链一行不少</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          <p className="text-muted-foreground text-sm">
            退役是「这条策略不再参与检索」的终态之一；墓园与复活的前提就在这里 ——
            退役之后策略行与它的证据链都还在，随时可查。
          </p>
          {retirable ? (
            <div>
              <Button variant="destructive" onClick={() => setRetireOpen(true)}>
                <ArchiveIcon /> 退役这条策略
              </Button>
            </div>
          ) : (
            <p className="text-muted-foreground text-sm">
              当前状态（{strategy.state}）不可退役 —— 后端只允许从隔离态 / 在役态退役。
            </p>
          )}
        </CardContent>
      </Card>

      <p className="text-muted-foreground text-xs">
        <ShieldCheckIcon className="mr-1 inline size-3.5 align-text-bottom" />
        裁决写只在宿主侧发生：判据是状态比 + 必填字段，没有一处问模型；每一次动作都会进 audit_events 哈希链。
      </p>

      <DecisionDialog
        open={pinTarget !== null}
        onOpenChange={(next) => {
          if (!next) setPinTarget(null);
        }}
        title={pinTarget ? "置位 pin" : "解除 pin"}
        description={
          pinTarget
            ? "置位意味着「这条策略是通往某个 confirmed 漏洞的唯一已知路径」，不许被自动淘汰。"
            : "解除 pin 会让依赖这条路径的漏洞报告失去复现途径 —— 理由与操作人都会进审计哈希链。"
        }
        confirmLabel={pinTarget ? "置位 pin" : "解除 pin"}
        destructive={pinTarget === false}
        onConfirm={confirmPin}
        onDone={onChanged}
      />

      <DecisionDialog
        open={retireOpen}
        onOpenChange={setRetireOpen}
        title="退役策略"
        description="退役不是删除：策略行与证据链都会保留，只是不再参与检索。理由与操作人进审计哈希链。"
        confirmLabel="确认退役"
        destructive
        acknowledge="我确认退役这条策略（它将不再参与检索，但行与证据链会保留）"
        onConfirm={confirmRetire}
        onDone={onChanged}
      />
    </div>
  );
}
