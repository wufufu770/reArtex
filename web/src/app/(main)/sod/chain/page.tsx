"use client";

import { Link2Icon } from "lucide-react";

import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";

import { ChainStatusCard } from "../_components/chain-status-card";
import { SodNav } from "../_components/sod-nav";

export default function SodChainPage() {
  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div>
        <h1 className="font-semibold text-xl tracking-tight">审计链校验</h1>
        <p className="text-muted-foreground text-sm">
          审计记录逐条串成哈希链 · 链被改过一定验得出来 · 校验只读，不会顺手把断链修好
        </p>
      </div>

      <SodNav current="chain" />

      <ChainStatusCard />


      <Card>
        <CardHeader>
          <CardTitle>这条链是什么</CardTitle>
          <CardDescription>
            每一笔裁决（放行 / 拒绝 / 待证伪）都往 audit_events 里追加一条记录，这条记录的摘要依赖上一条的摘要
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-3 text-muted-foreground text-xs">
          <p>
            所以链是一根从头串到尾的绳：改动中间任何一条，后面所有记录的摘要都对不上，校验会指出**第一处**断裂的位置。
            这正是「决策账本能不能当证据用」的判据 —— 链断了，账本里的裁决就不再是可信的记录。
          </p>
          <p>
            ⚠️ 断链是一个**结论**，不是接口故障：校验接口照常返回，断了就是断了。所以页面上它是「链已断裂」这个状态，
            而不是一句「读取失败」。
          </p>
          <p>
            校验是只读的：一个 GET 绝不会把断链「修好」—— 补一条记录或者改一条记录都会让链更不可信。
            真要追查，得去看断点前后那几条记录是不是被绕过写入路径改过。
          </p>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>链完整 ≠ 闸门管住了全部出网</CardTitle>
          <CardDescription>审计链只回答「记录有没有被改过」，不回答「该拦的都拦住了」</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-2 text-muted-foreground text-xs">
          <p>
            链完整说明的是这批记录没被篡改，不说明闸门的覆盖是完整的：平台的出网治理按设计就是**部分**的
            （上面那块出网治理状态里列着未覆盖通道）。审计链再完整，也只能证明「没被记录的事」不存在，
            不能证明「没被记录的事」没发生。
          </p>
          <p>
            想知道某一次动作本身是不是被管住了，要去
            <span className="mx-0.5 font-medium text-foreground">决策账本</span>
            看它落在哪一层、拿到的裁决是什么。
          </p>
        </CardContent>
      </Card>

      <p className="text-muted-foreground text-xs">
        <Link2Icon className="mr-1 inline size-3.5 align-text-bottom" />
        链上的每一笔都对应账本里的一条裁决 —— 想从某条裁决往回查，改动发生在哪一次，可以对到 决策账本的时间线上。
      </p>
    </div>
  );
}
