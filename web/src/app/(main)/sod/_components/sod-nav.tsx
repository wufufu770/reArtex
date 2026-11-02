import Link from "next/link";

import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";

// SodNav 是 SoD 链四个观测面之间的页签导航。
//
// 页签用链接而不是本地状态：这张面是给人做审计追溯的，「把这条裁决的账本
// 链接发给别人」「把断链的位置发给别人」经常要单独发地址，所以每一面都得有
// 自己的地址。
export function SodNav({ current }: { current: "decisions" | "review-queue" | "circuit" | "chain" }) {
  const items = [
    { value: "decisions", label: "决策账本", href: "/sod" },
    { value: "review-queue", label: "对抗复核队列", href: "/sod/review-queue" },
    { value: "circuit", label: "工具熔断", href: "/sod/circuit" },
    { value: "chain", label: "审计链校验", href: "/sod/chain" },
  ] as const;

  return (
    <Tabs value={current}>
      <TabsList variant="default" className="min-w-max">
        {items.map((it) => (
          <TabsTrigger key={it.value} value={it.value} asChild>
            <Link href={it.href}>{it.label}</Link>
          </TabsTrigger>
        ))}
      </TabsList>
    </Tabs>
  );
}
