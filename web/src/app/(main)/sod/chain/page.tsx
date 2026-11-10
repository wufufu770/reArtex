"use client";

import * as React from "react";

import { ChevronLeftIcon, ChevronRightIcon, Link2Icon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { api } from "@/lib/api";
import type { AuditEvent } from "@/lib/types";

import { ChainStatusCard } from "../_components/chain-status-card";
import { SodNav } from "../_components/sod-nav";

// 审计事件单页条数（后端上限 200，50 条一页足够翻查明细）。
const PAGE_SIZE = 50;
// payload 明细的展示截断：后端本身存的是前 240 字，表格里再截一次保证行高稳定，
// 悬停可看后端保留的全文。
const PAYLOAD_LIMIT = 120;

// fmtPayload 把 payload 明细截到表格展示长度；后端保留的全文放 title 悬浮。
function fmtPayload(text: string): string {
  if (!text) return "—";
  if (text.length <= PAYLOAD_LIMIT) return text;
  return `${text.slice(0, PAYLOAD_LIMIT)}…`;
}

// AuditEventList 是明细表的渲染：加载中 / 空态 / 表格三种。
function AuditEventList({ events, loaded }: { events: AuditEvent[]; loaded: boolean }) {
  if (!loaded) {
    return <p className="py-6 text-center text-muted-foreground text-sm">加载中…</p>;
  }
  if (events.length === 0) {
    return (
      <p className="py-6 text-center text-muted-foreground text-sm">
        链上还没有事件——第一次人工裁决 / 策略裁决 / 额度变更发生后这里会有第一条。
      </p>
    );
  }
  return (
    <div className="max-h-[60svh] overflow-y-auto rounded-md border">
      <Table>
        <TableHeader className="sticky top-0 z-10 bg-card">
          <TableRow>
            <TableHead className="w-16">seq</TableHead>
            <TableHead className="w-32">时间</TableHead>
            <TableHead>actor</TableHead>
            <TableHead>tool</TableHead>
            <TableHead className="w-36">action</TableHead>
            <TableHead>reason</TableHead>
            <TableHead className="hidden lg:table-cell">payload</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {events.map((row) => (
            <TableRow key={row.id}>
              <TableCell>
                <span className="font-mono text-muted-foreground text-xs tabular-nums">{row.seq}</span>
              </TableCell>
              <TableCell>
                <span className="whitespace-nowrap text-muted-foreground text-xs tabular-nums">{row.ts}</span>
              </TableCell>
              <TableCell>
                <span className="font-mono text-xs" title={row.actor}>
                  {row.actor || "—"}
                </span>
              </TableCell>
              <TableCell>
                <span className="font-mono text-xs" title={row.tool}>
                  {row.tool || "—"}
                </span>
              </TableCell>
              <TableCell>
                <span className="break-all font-mono text-xs">{row.action || "—"}</span>
              </TableCell>
              <TableCell className="max-w-xs">
                <span className="block truncate text-muted-foreground text-xs" title={row.reason}>
                  {row.reason || "—"}
                </span>
              </TableCell>
              <TableCell className="hidden lg:table-cell">
                <div
                  className="max-w-md truncate font-mono text-[10px] text-muted-foreground"
                  title={row.payload || "—"}
                >
                  {fmtPayload(row.payload)}
                </div>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}

export default function SodChainPage() {
  const [events, setEvents] = React.useState<AuditEvent[]>([]);
  const [total, setTotal] = React.useState(0);
  const [page, setPage] = React.useState(1);
  const [loaded, setLoaded] = React.useState(false);

  const lastPage = Math.max(1, Math.ceil(total / PAGE_SIZE));

  React.useEffect(() => {
    let alive = true;
    api
      .auditEvents(page, PAGE_SIZE)
      .then((r) => {
        if (!alive) return;
        setEvents(r.items);
        setTotal(r.total);
      })
      .catch((e) => {
        if (alive) toast.error(`读取审计事件失败：${(e as Error).message}`);
      })
      .finally(() => {
        if (alive) setLoaded(true);
      });
    return () => {
      alive = false;
    };
  }, [page]);

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

      {/* E1-8：audit_events 哈希链的明细页——此前链只有「校验通过/断在几号」的结论，
          单条事件记了什么、谁做的、为什么，没有地方可看。与上方链校验读同一张表。 */}
      <Card>
        <CardHeader>
          <CardTitle>审计事件（哈希链明细）</CardTitle>
          <CardDescription>
            与上方链校验同源，append-only · 共 {total} 条，按 seq 倒序 · payload 只保留写入时的前 240 字
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          <AuditEventList events={events} loaded={loaded} />
          <div className="flex items-center gap-2">
            <span className="text-muted-foreground text-xs tabular-nums">
              第 {page} / {lastPage} 页 · 每页 {PAGE_SIZE} 条
            </span>
            <div className="ml-auto flex gap-2">
              <Button
                variant="outline"
                size="sm"
                disabled={!loaded || page <= 1}
                onClick={() => setPage((p) => Math.max(1, p - 1))}
              >
                <ChevronLeftIcon /> 上一页
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={!loaded || page >= lastPage}
                onClick={() => setPage((p) => p + 1)}
              >
                下一页 <ChevronRightIcon />
              </Button>
            </div>
          </div>
        </CardContent>
      </Card>

      <p className="text-muted-foreground text-xs">
        <Link2Icon className="mr-1 inline size-3.5 align-text-bottom" />
        链上的每一笔都对应账本里的一条裁决 —— 想从某条裁决往回查，改动发生在哪一次，可以对到 决策账本的时间线上。
      </p>
    </div>
  );
}
