"use client";

import * as React from "react";

import { FileTextIcon, PencilIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Spinner } from "@/components/ui/spinner";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type { Strategy } from "@/lib/types";

import { fmtJSON } from "../../_components/strategy-format";

// 正文列 = 策略的六元组 𝒜 ℐ ℂ 𝒯 π ℰ 加上适用范围。
// 顺序按「先说什么时候用 → 怎么做 → 依赖什么 → 什么算成功 / 失败」排。
const BODY_FIELDS: { key: keyof Strategy; label: string; hint: string }[] = [
  { key: "activation", label: "激活条件 𝒜", hint: "资产图谓词：什么目标才适用这条策略" },
  { key: "steps", label: "步骤 ℐ", hint: "有序步骤，每步含工具与参数模板" },
  { key: "preconditions", label: "前置条件 ℂ", hint: "开打之前必须先满足的东西" },
  { key: "tool_requirements", label: "工具需求 𝒯", hint: "这条策略要用哪些工具" },
  { key: "success_criteria", label: "里程碑 π", hint: "命中什么算成功" },
  { key: "failure_modes", label: "已知失败模式 ℰ", hint: "没触发过的里程碑会自动落到这里" },
  { key: "parameters", label: "参数 input_arguments", hint: "参数与默认值" },
  { key: "applies_to", label: "适用范围", hint: "资产图谓词的第二组键" },
];

// draftJSON 把当前值序列化成可回填的原始 JSON 文本（编辑模式用）。
// 与 fmtJSON 不同：这里是往返用的规范文本，空值给空串；jsonb 里的字符串值
// 会带引号（否则保存时 JSON.parse 过不去）。
function draftJSON(value: unknown): string {
  if (value === undefined || value === null) return "";
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return "";
  }
}

// BodyFieldReadonly 渲染单个字段的只读形态（空值提示 / 原样 JSON 文本）。
// 拆出来是为了让「编辑 or 只读」只留一层三元。
function BodyFieldReadonly({ text }: { text: string }) {
  if (text === "—") return <p className="text-muted-foreground text-xs">（这一项是空的）</p>;
  return (
    <pre className="max-h-96 overflow-auto whitespace-pre-wrap rounded-md bg-muted/50 p-3 font-mono text-xs">
      {text}
    </pre>
  );
}

// BodyTab 展示策略正文（只读为默认，另提供版本化的编辑模式）。
//
// ⚠️ 正文是**参考文本**：策略不会被自动执行，页面上也不提供任何「跑一下」的入口。
export function BodyTab({ strategy, onSaved }: { strategy: Strategy; onSaved?: () => void }) {
  const [editing, setEditing] = React.useState(false);
  // 每字段一份原始 JSON 文本缓冲；进入编辑时按当前值初始化。
  const [drafts, setDrafts] = React.useState<Record<string, string>>({});
  const [saving, setSaving] = React.useState(false);

  function startEdit() {
    const next: Record<string, string> = {};
    for (const f of BODY_FIELDS) next[String(f.key)] = draftJSON(strategy[f.key]);
    setDrafts(next);
    setEditing(true);
  }

  // 保存 = 把每个非空字段 parse 后交给 PATCH /strategies/{id}（版本化写）。
  // 空文本 = 不动这一列；想清空某列显式填 null。任何一列 JSON 非法就地拦下。
  async function saveEdit() {
    const patchBody: Record<string, unknown> = {};
    for (const f of BODY_FIELDS) {
      const text = (drafts[String(f.key)] ?? "").trim();
      if (!text) continue;
      try {
        patchBody[f.key] = JSON.parse(text);
      } catch {
        toast.error(`${f.label} 不是合法 JSON，改好再保存`);
        return;
      }
    }
    if (Object.keys(patchBody).length === 0) {
      toast.error("没有改动任何字段 —— 至少填一项 JSON 再保存（清空列请显式填 null）");
      return;
    }
    setSaving(true);
    try {
      const r = await api.strategyEdit(strategy.id, patchBody);
      toast.success(`已保存为 v${r.version}`);
      setEditing(false);
      onSaved?.();
    } catch (e) {
      toast.error(`保存失败：${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <CardHeader className="flex flex-row items-start justify-between">
          <div className="flex flex-col gap-1.5">
            <CardTitle>策略正文</CardTitle>
            <CardDescription>正文是给人读的说明书 —— 策略库不会自动执行任何一条</CardDescription>
          </div>
          {editing ? (
            <div className="flex items-center gap-2">
              <Button variant="outline" size="sm" onClick={() => setEditing(false)} disabled={saving}>
                取消
              </Button>
              <Button size="sm" onClick={() => void saveEdit()} disabled={saving}>
                {saving && <Spinner data-icon="inline-start" />}
                {saving ? "保存中…" : "保存"}
              </Button>
            </div>
          ) : (
            <Button variant="outline" size="sm" onClick={startEdit}>
              <PencilIcon />
              编辑
            </Button>
          )}
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          {BODY_FIELDS.map((f) => {
            const text = fmtJSON(strategy[f.key]);
            return (
              <div key={String(f.key)} className="flex flex-col gap-1">
                <div className="flex flex-wrap items-baseline gap-2">
                  <span className="font-medium text-sm">{f.label}</span>
                  <span className="text-muted-foreground text-xs">{f.hint}</span>
                </div>
                {editing ? (
                  <Textarea
                    className="min-h-24 font-mono text-xs"
                    value={drafts[String(f.key)] ?? ""}
                    onChange={(e) => setDrafts((cur) => ({ ...cur, [String(f.key)]: e.target.value }))}
                    placeholder="原始 JSON 文本；留空 = 不改动（清空该列填 null）"
                  />
                ) : (
                  <BodyFieldReadonly text={text} />
                )}
              </div>
            );
          })}
        </CardContent>
      </Card>

      <p className="text-muted-foreground text-xs">
        <FileTextIcon className="mr-1 inline size-3.5 align-text-bottom" />
        正文由人写入，形状随写入方而变；库里没有真实目标地址，参数一律是模板。
      </p>
    </div>
  );
}
