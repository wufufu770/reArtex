import Link from "next/link";

import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";

// StrategyNav 是策略库三个面之间的页签导航。
//
// 页签用链接而不是本地状态：策略库是人工复核面，「这条提议待裁」「这个 pin 该
// 复审了」经常要单独把链接发给别人，所以每一面都得有自己的地址。
export function StrategyNav({ current }: { current: "strategies" | "proposals" | "pin-queue" }) {
  const items = [
    { value: "strategies", label: "策略", href: "/strategies" },
    { value: "proposals", label: "待裁决提议", href: "/strategies/proposals" },
    { value: "pin-queue", label: "pin 复审队列", href: "/strategies/pin-queue" },
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
