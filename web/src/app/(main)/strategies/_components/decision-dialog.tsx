"use client";

import * as React from "react";

import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Spinner } from "@/components/ui/spinner";
import { Textarea } from "@/components/ui/textarea";

// ACTOR_KEY 记住上一次的操作人 —— 裁决多半是同一个人连续做几笔，
// 每次重打一遍名字只会让人不想填。
const ACTOR_KEY = "artex_strategy_actor";

export function recallActor(): string {
  if (typeof window === "undefined") return "";
  return window.localStorage.getItem(ACTOR_KEY) ?? "";
}

function rememberActor(actor: string): void {
  if (typeof window === "undefined") return;
  try {
    window.localStorage.setItem(ACTOR_KEY, actor);
  } catch {
    // localStorage 不可用（隐私模式）时忽略：只是少一次免填，不影响裁决本身。
  }
}

// DecisionDialog 是策略库**所有人工写动作**的统一表单。
//
// 策略库的四个写动作（置位 / 解除 pin、退役、裁决提议）都是「裁决写」：
// 后端要求理由与操作人**都**非空，缺一个就回 400；这一笔还会进
// audit_events 哈希链，事后能查「谁做的、为什么」。
// 所以这两个字段在 UI 上就是必填项（填齐之前提交按钮禁用），
// 而不是把后端的 400 抛给用户看。
export function DecisionDialog({
  open,
  onOpenChange,
  title,
  description,
  confirmLabel,
  destructive = false,
  acknowledge,
  onConfirm,
  onDone,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description: string;
  confirmLabel: string;
  /** 危险动作（退役）用 destructive 按钮样式。 */
  destructive?: boolean;
  /** 危险动作的第二道确认：给了这句话就必须勾选它才能提交。 */
  acknowledge?: string;
  /** 失败时 throw，弹窗保持打开并把错误弹出来。 */
  onConfirm: (reason: string, actor: string) => Promise<void>;
  /** 成功后回调（父组件在这里重取数据）。 */
  onDone?: () => void;
}) {
  const [reason, setReason] = React.useState("");
  const [actor, setActor] = React.useState("");
  const [acked, setAcked] = React.useState(false);
  const [saving, setSaving] = React.useState(false);

  React.useEffect(() => {
    if (open) setActor(recallActor());
  }, [open]);

  const canSubmit = reason.trim().length > 0 && actor.trim().length > 0 && (!acknowledge || acked) && !saving;

  function reset() {
    setReason("");
    setActor("");
    setAcked(false);
  }

  async function submit() {
    if (!canSubmit) return;
    setSaving(true);
    try {
      await onConfirm(reason.trim(), actor.trim());
      rememberActor(actor.trim());
      onOpenChange(false);
      reset();
      onDone?.();
    } catch (e) {
      toast.error(`操作失败：${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        onOpenChange(next);
        if (!next) reset();
      }}
    >
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription>{description}</DialogDescription>
        </DialogHeader>

        <div className="grid gap-4 py-2">
          <div className="grid gap-1.5">
            <Label htmlFor="decision-reason">理由</Label>
            <Textarea
              id="decision-reason"
              placeholder="如：该路径已是某 confirmed 漏洞的唯一已知路径，不再变更"
              value={reason}
              onChange={(e) => setReason(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">理由必填 —— 这一笔要进审计哈希链，没有理由事后追不回来。</p>
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="decision-actor">操作人</Label>
            <Input
              id="decision-actor"
              placeholder="如：张三 / zhangsan"
              value={actor}
              onChange={(e) => setActor(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">操作人必填 —— 审计链要记下是谁做的。</p>
          </div>
          {acknowledge && (
            <label
              htmlFor="decision-ack"
              className="flex cursor-pointer items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-2.5"
            >
              <Checkbox id="decision-ack" checked={acked} onCheckedChange={(v) => setAcked(v === true)} />
              <span className="text-sm">{acknowledge}</span>
            </label>
          )}
        </div>

        <DialogFooter>
          <Button variant={destructive ? "destructive" : "default"} onClick={() => void submit()} disabled={!canSubmit}>
            {saving && <Spinner data-icon="inline-start" />}
            {saving ? "提交中" : confirmLabel}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
