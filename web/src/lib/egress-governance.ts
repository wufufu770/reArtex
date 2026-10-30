// 出网治理状态 → 展示模型。
//
// 这个模块是纯函数，不碰 React、不发请求，所以「不完整 / 未知 / 完整」三种状态
// 可以被单独测。组件只负责把结果画出来。
//
// 不变式（与后端 server/egress_governance.go 一致）：未覆盖通道清单非空 ⇒ 状态
// 一定是「不完整」。这里【不读】后端给的 governed 字段，只按清单派生——所以 UI
// 不可能在清单还列着未覆盖通道时说出「已管控」这种话。

import type { Tone } from "./status";
import type { EgressGovernanceState } from "./types";

export type EgressGovernanceStatus = "incomplete" | "unknown" | "complete";

export interface EgressGovernanceView {
  status: EgressGovernanceStatus;
  // 配色档位，取自 lib/status.ts 的 Tone 体系，不另起一套颜色。
  tone: Tone;
  // 状态标题。
  label: string;
  // 徽标文字，带一个会变的值（条数 / 未知），不是装饰。
  badge: string;
  // 一句话交代这份状态是怎么来的。
  summary: string;
  // 未覆盖通道原文，逐条照后端给的字符串显示，不改写、不翻译、不合并。
  // unknown / complete 时为空数组。
  channels: string[];
  // 后端报告的未覆盖通道条数；null = 未知（不是 0）。
  channelCount: number | null;
}

const UNKNOWN_VIEW: EgressGovernanceView = {
  status: "unknown",
  tone: "slate",
  label: "出网治理状态未知",
  badge: "状态未知",
  summary: "未能读取出网治理状态接口，出网覆盖情况无法确认。",
  channels: [],
  channelCount: null,
};

const COMPLETE_VIEW: EgressGovernanceView = {
  status: "complete",
  tone: "green",
  label: "出网治理完整",
  badge: "0 条未覆盖通道",
  summary: "后端报告没有未覆盖通道。",
  channels: [],
  channelCount: 0,
};

export function egressGovernanceView(state: EgressGovernanceState | null | undefined): EgressGovernanceView {
  const raw: unknown = state?.uncovered_channels;
  // 清单不是数组 = 响应不可用（接口未注册、返回了别的形状）。这跟「清单为空」是
  // 两回事：前者是未知，后者才是完整。
  if (!Array.isArray(raw)) return UNKNOWN_VIEW;
  if (raw.length === 0) return COMPLETE_VIEW;

  const channels = raw.filter((item): item is string => typeof item === "string" && item.trim() !== "");
  return {
    status: "incomplete",
    tone: "amber",
    label: "出网治理不完整",
    badge: `${raw.length} 条未覆盖通道`,
    summary: "后端报告以下通道未被出网范围校验覆盖：",
    channels,
    channelCount: raw.length,
  };
}
