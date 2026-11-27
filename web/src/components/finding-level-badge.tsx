import { Badge } from "@/components/ui/badge";
import type { Finding } from "@/lib/types";

// FindingLevelBadges 展示影响层级、证据等级，以及 critical/high 证据不足。
// 任务内旧节点没有这几列时什么都不画。
export function FindingLevelBadges({
  finding,
}: {
  finding: Pick<Finding, "impact_level" | "evidence_level" | "evidence_insufficient" | "evidence_level_reason">;
}) {
  const impact = finding.impact_level ?? 0;
  const evidence = finding.evidence_level ?? 0;
  if (impact < 1 && evidence < 1 && !finding.evidence_insufficient) {
    return null;
  }
  return (
    <>
      {impact >= 1 && (
        <Badge variant="outline" className="text-[10px]">
          影响 L{impact}
        </Badge>
      )}
      {evidence >= 1 && (
        <Badge variant="outline" className="text-[10px]">
          证据 L{evidence}
        </Badge>
      )}
      {finding.evidence_insufficient && (
        <Badge
          variant="outline"
          className="text-[10px] text-amber-700"
          title={finding.evidence_level_reason || "critical/high 需要 L3 及以上证据"}
        >
          证据不足
        </Badge>
      )}
    </>
  );
}
