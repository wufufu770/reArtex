import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";

// 任务难度、评级、最高影响层级。颜色与 C-06 卡面一致。
// 空难度或未评级显示「—」，没有 finding 时最高影响层级也是「—」。

const DIFFICULTY_CLASS: Record<string, string> = {
  easy: "text-muted-foreground",
  medium: "text-sky-700 dark:text-sky-300",
  hard: "text-orange-700 dark:text-orange-300",
  expert: "text-red-700 dark:text-red-300",
};

const GRADE_CLASS: Record<string, string> = {
  S: "border-amber-500/60 text-amber-700 dark:text-amber-300",
  A: "text-emerald-700 dark:text-emerald-300",
  B: "text-sky-700 dark:text-sky-300",
  C: "text-muted-foreground",
};

export function DifficultyBadge({ d, title }: { d?: string; title?: string }) {
  const cls = d ? DIFFICULTY_CLASS[d] : undefined;
  if (!cls) return <span className="text-muted-foreground">—</span>;
  return (
    <Badge variant="outline" className={cn("text-[10px]", cls)} title={title}>
      {d}
    </Badge>
  );
}

export function GradeBadge({ g, title }: { g?: string; title?: string }) {
  if (!g) return <span className="text-muted-foreground">—</span>;
  const cls = GRADE_CLASS[g];
  if (!cls) return <span className="text-muted-foreground">—</span>;
  return (
    <Badge variant="outline" className={cn("text-[10px]", cls)} title={title}>
      {g}
    </Badge>
  );
}

export function ImpactBadge({ level, title }: { level?: number; title?: string }) {
  if (level == null || level < 1) return <span className="text-muted-foreground">—</span>;
  const n = Math.min(4, Math.floor(level));
  return (
    <Badge variant="outline" className="text-[10px]" title={title}>
      L{n}
    </Badge>
  );
}
