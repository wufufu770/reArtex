"use client";

import { FileTextIcon } from "lucide-react";

import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
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

// BodyTab 展示策略正文。
//
// ⚠️ 正文是**参考文本**：策略不会被自动执行，页面上也不提供任何「跑一下」的入口。
export function BodyTab({ strategy }: { strategy: Strategy }) {
  return (
    <div className="flex flex-col gap-4">
      <Card>
        <CardHeader>
          <CardTitle>策略正文</CardTitle>
          <CardDescription>正文是给人读的说明书 —— 策略库不会自动执行任何一条</CardDescription>
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
                {text === "—" ? (
                  <p className="text-muted-foreground text-xs">（这一项是空的）</p>
                ) : (
                  <pre className="max-h-96 overflow-auto whitespace-pre-wrap rounded-md bg-muted/50 p-3 font-mono text-xs">
                    {text}
                  </pre>
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
