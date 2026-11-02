import { cn } from "@/lib/utils";

// QuotaMeter renders 消费 + 预扣 against the budget as one stacked bar.
//
// 为什么不用 ui/progress：Progress 只能表达一个值，而额度要同时说清「已结算」
// 「在途预扣」「剩余」三笔账 —— 合成一根柱会把在途预扣那一栏盖掉。
// 视觉参数沿用 Progress 的写法（rounded-full / bg-muted 轨道），颜色取自
// lib/status.ts 已有的语义色（主色=已结算，琥珀=在途预扣，红=超额）。
export function QuotaMeter({
  consumed,
  reserved,
  budget,
  className,
}: {
  consumed: number;
  reserved: number;
  budget: number;
  className?: string;
}) {
  const used = Math.max(0, consumed) + Math.max(0, reserved);
  // 额度用尽后再有消费时，剩余为负：分母取三者最大值，柱子仍然画满并转红。
  const scale = Math.max(budget, used, 1);
  const consumedPct = (Math.max(0, consumed) / scale) * 100;
  const reservedPct = (Math.max(0, reserved) / scale) * 100;
  const over = budget > 0 && used > budget;

  return (
    <div className={cn("h-1.5 w-full overflow-hidden rounded-full bg-muted", className)}>
      <div className="flex h-full w-full">
        <div className={cn("h-full", over ? "bg-red-500" : "bg-primary")} style={{ width: `${consumedPct}%` }} />
        <div className="h-full bg-amber-500" style={{ width: `${reservedPct}%` }} />
      </div>
    </div>
  );
}
