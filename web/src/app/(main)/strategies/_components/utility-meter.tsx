import { cn } from "@/lib/utils";

// UtilityMeter renders a 0..1 utility as one thin bar.
//
// utility 的权威值在 strategy_utility 物化视图里，策略表上的那一列只是缓存；
// 这里只表达「相对高低」，不表达任何判据 —— 淘汰阈值（τ_u）由后端的 curator 判。
export function UtilityMeter({ value, className }: { value: number; className?: string }) {
  const ratio = Number.isFinite(value) ? Math.max(0, Math.min(1, value)) : 0;
  return (
    <div className={cn("h-1.5 w-full overflow-hidden rounded-full bg-muted", className)}>
      <div className="h-full bg-primary" style={{ width: `${ratio * 100}%` }} />
    </div>
  );
}
