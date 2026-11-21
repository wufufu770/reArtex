"use client";

import * as React from "react";

import { DatabaseIcon, ExternalLinkIcon, GaugeIcon, RefreshCwIcon, SearchIcon, ShieldCheckIcon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api } from "@/lib/api";
import type { AmberMemoryHit, AmberMemoryItem, SelfEvolveStatus, VetoExport } from "@/lib/types";

// 自进化（RSI）面板（0.15.41）：把两个外挂组件的运行态放进 UI——
//   ① Amber 外置永久记忆库：开关/地址（settings 面）+ 探活 + 记忆浏览
//     （正式记忆 / agent 隔离区 / 语义检索，全部只读——晋升走 curator
//     观察态与人工，J3 前零自动落写）；
//   ② Veto agent 测评器：终态任务的 reartex_pentest_v1 导出清单（六字段
//     协议 + sod_chain_valid 徽章），评分本身跑在外置 veto-eval CLI。
// 自动改进闭环（J3）仍是推迟项：本页只做「看得见」，不做「自动改」。

function fmtTime(v?: string | number | null): string {
  if (v === null || v === undefined || v === "") return "—";
  const d = typeof v === "number" ? new Date(v * 1000) : new Date(v);
  if (Number.isNaN(d.getTime())) return String(v);
  return d.toLocaleString();
}

export default function SelfEvolvePage() {
  const [status, setStatus] = React.useState<SelfEvolveStatus | null>(null);
  const [statusLoading, setStatusLoading] = React.useState(false);

  // Amber 配置表单（与通知推送页同 settings 面；两处任一保存即时生效）。
  const [amberEnabled, setAmberEnabled] = React.useState(false);
  const [amberURL, setAmberURL] = React.useState("");
  const [amberSaving, setAmberSaving] = React.useState(false);

  // 记忆浏览。
  const [items, setItems] = React.useState<AmberMemoryItem[]>([]);
  const [quarantine, setQuarantine] = React.useState<AmberMemoryItem[]>([]);
  const [query, setQuery] = React.useState("");
  const [hits, setHits] = React.useState<AmberMemoryHit[] | null>(null);
  const [browsing, setBrowsing] = React.useState(false);

  // Veto 导出。
  const [exports, setExports] = React.useState<VetoExport[]>([]);

  const loadStatus = React.useCallback(async () => {
    setStatusLoading(true);
    try {
      const s = await api.selfEvolveStatus();
      setStatus(s);
      setAmberEnabled(s.amber?.enabled ?? false);
      setAmberURL(s.amber?.base_url ?? "");
    } catch (e) {
      toast.error(`状态加载失败：${(e as Error).message}`);
    } finally {
      setStatusLoading(false);
    }
  }, []);

  const loadExports = React.useCallback(async () => {
    try {
      const r = await api.vetoExports();
      setExports(r.exports ?? []);
    } catch (e) {
      toast.error(`测评导出加载失败：${(e as Error).message}`);
    }
  }, []);

  React.useEffect(() => {
    void loadStatus();
    void loadExports();
  }, [loadStatus, loadExports]);

  // loadBrowse 按当前 tab 拉数据（切换/刷新时调用）。
  const loadBrowse = async (tab: string) => {
    setBrowsing(true);
    try {
      if (tab === "items") {
        const r = await api.amberItems(50);
        setItems(r.items ?? []);
      } else if (tab === "quarantine") {
        const r = await api.amberQuarantine();
        setQuarantine(r.items ?? []);
      } else if (tab === "search" && hits === null) {
        setHits([]);
      }
    } catch (e) {
      toast.error(`记忆读取失败：${(e as Error).message}`);
    } finally {
      setBrowsing(false);
    }
  };

  const searchMemories = async () => {
    if (!query.trim()) return;
    setBrowsing(true);
    try {
      const r = await api.amberMemorySearch(query.trim(), 10);
      setHits(r.hits ?? []);
    } catch (e) {
      toast.error(`检索失败：${(e as Error).message}`);
    } finally {
      setBrowsing(false);
    }
  };

  async function saveAmber() {
    setAmberSaving(true);
    try {
      await api.setSettings({ amber_enabled: amberEnabled, amber_base_url: amberURL.trim() });
      toast.success("记忆库配置已保存（即时生效）");
      await loadStatus();
    } catch (e) {
      toast.error("保存失败：" + (e as Error).message);
    } finally {
      setAmberSaving(false);
    }
  }

  const amber = status?.amber;
  const health = status?.amber_health;
  const curator = status?.curator;
  const observation = curator?.observation ?? null;

  return (
    <div className="mx-auto flex w-full max-w-5xl flex-col gap-4 p-4">
      {/* ① 总览 */}
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <RefreshCwIcon className="size-4 text-primary" /> 自进化（RSI）· 运行总览
            <Button
              variant="outline"
              size="sm"
              className="ml-auto"
              onClick={() => void loadStatus()}
              disabled={statusLoading}
            >
              <RefreshCwIcon className={statusLoading ? "animate-spin" : ""} /> 刷新
            </Button>
          </CardTitle>
        </CardHeader>
        <CardContent className="grid gap-3 sm:grid-cols-3">
          <div className="rounded-md border p-3">
            <p className="text-muted-foreground mb-1 text-xs">外置记忆库（Amber）</p>
            <div className="flex items-center gap-2">
              <Badge variant={amber?.enabled ? "default" : "secondary"}>{amber?.enabled ? "已启用" : "未启用"}</Badge>
              {amber?.enabled && (
                <Badge variant={health?.ok ? "default" : "destructive"}>{health?.ok ? "服务在线" : "不可达"}</Badge>
              )}
            </div>
            <p className="text-muted-foreground mt-1 text-xs">{health?.detail ?? "—"}</p>
            <p className="text-muted-foreground mt-1 text-xs">
              令牌：owner {amber?.owner_token_set ? "✓" : "✗"} · worker {amber?.worker_token_set ? "✓" : "✗"}
              （环境变量面）
            </p>
          </div>
          <div className="rounded-md border p-3">
            <p className="text-muted-foreground mb-1 text-xs">记忆 curator（观察态）</p>
            <p className="text-sm">
              上次观察：{fmtTime(curator?.last_run && curator.last_run > 0 ? curator.last_run : null)}
            </p>
            <p className="text-muted-foreground mt-1 text-xs">
              {observation
                ? `隔离区 ${observation.quarantine_total ?? 0} 条 · 拟晋升 ${(observation.propose_promote ?? []).length} · 拟人工 ${(observation.propose_review ?? []).length}`
                : "尚无观察数据（启用后每小时一轮）"}
            </p>
          </div>
          <div className="rounded-md border p-3">
            <p className="text-muted-foreground mb-1 text-xs">agent 测评器（Veto）</p>
            <p className="text-sm">已导出任务：{status?.veto?.export_count ?? 0}</p>
            <p className="text-muted-foreground mt-1 text-xs">
              {status?.veto?.latest_task_ids?.length
                ? `最近任务：${status.veto.latest_task_ids.join("、")}`
                : "暂无终态导出"}
            </p>
          </div>
        </CardContent>
      </Card>

      {/* ② Amber 配置 + 记忆浏览 */}
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <DatabaseIcon className="size-4 text-primary" /> 外置永久记忆库
            <span className="text-muted-foreground text-xs font-normal">
              worker 经 MCP 通道写入（强制隔离区）；本面板只读浏览
            </span>
          </CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          <div className="flex flex-wrap items-end gap-4">
            <div className="flex items-center gap-2">
              <Switch id="se-amber-enabled" checked={amberEnabled} onCheckedChange={setAmberEnabled} />
              <Label htmlFor="se-amber-enabled" className="text-sm">
                启用外置记忆库
              </Label>
            </div>
            <div className="flex min-w-64 flex-1 flex-col gap-1">
              <Label htmlFor="se-amber-url">服务地址</Label>
              <Input
                id="se-amber-url"
                placeholder="http://127.0.0.1:8002"
                value={amberURL}
                onChange={(e) => setAmberURL(e.target.value)}
              />
            </div>
            <Button onClick={saveAmber} disabled={amberSaving}>
              {amberSaving ? "保存中…" : "保存配置"}
            </Button>
          </div>
          <p className="text-muted-foreground text-xs">
            令牌为环境变量面（永不入库）：AMBER_OWNER_TOKEN（人工视角读写）/ AMBER_WORKER_TOKEN（agent 隔离区写入）/
            AMBER_CURATOR_TOKEN；Amber 开启 API 鉴权时至少配置其一。启动命令见下方指引。
          </p>

          <Tabs defaultValue="items" onValueChange={(v) => void loadBrowse(v)}>
            <TabsList>
              <TabsTrigger value="items">正式记忆</TabsTrigger>
              <TabsTrigger value="quarantine">隔离区（待审）</TabsTrigger>
              <TabsTrigger value="search">语义检索</TabsTrigger>
            </TabsList>
            <TabsContent value="items" className="flex flex-col gap-2">
              {items.map((it) => (
                <MemoryRow key={it.id ?? it.scope} item={it} />
              ))}
              {items.length === 0 && (
                <p className="text-muted-foreground text-sm">
                  {browsing ? "读取中…" : "暂无正式记忆（写入经隔离区，晋升后才出现在这里）"}
                </p>
              )}
            </TabsContent>
            <TabsContent value="quarantine" className="flex flex-col gap-2">
              {quarantine.map((it) => (
                <MemoryRow key={it.item_id ?? it.id ?? it.body} item={it} quarantined />
              ))}
              {quarantine.length === 0 && (
                <p className="text-muted-foreground text-sm">
                  {browsing ? "读取中…" : "隔离区为空（agent 通道写入的记忆先落在这里，等 curator/人工晋升）"}
                </p>
              )}
            </TabsContent>
            <TabsContent value="search" className="flex flex-col gap-2">
              <div className="flex items-center gap-2">
                <Input
                  placeholder="语义检索记忆库（回车执行）"
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter") void searchMemories();
                  }}
                />
                <Button onClick={() => void searchMemories()} disabled={browsing || !query.trim()}>
                  <SearchIcon /> 检索
                </Button>
              </div>
              {(hits ?? []).map((h) => (
                <div key={h.id ?? h.text} className="rounded-md border px-3 py-2">
                  <div className="flex flex-wrap items-center gap-2">
                    <Badge variant="outline">命中</Badge>
                    <span className="text-muted-foreground font-mono text-xs">{h.scope ?? "—"}</span>
                    <Badge variant="secondary" className="ml-auto">
                      score {h.score?.toFixed?.(3) ?? h.score}
                    </Badge>
                  </div>
                  <p className="mt-1 line-clamp-4 text-xs whitespace-pre-wrap">{h.text ?? "—"}</p>
                </div>
              ))}
              {hits !== null && hits.length === 0 && !browsing && (
                <p className="text-muted-foreground text-sm">无命中（或服务不可达）</p>
              )}
            </TabsContent>
          </Tabs>
        </CardContent>
      </Card>

      {/* ③ Veto 测评导出 */}
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <ShieldCheckIcon className="size-4 text-primary" /> agent 测评器（Veto）· 任务回执
            <span className="text-muted-foreground text-xs font-normal">
              终态自动导出 reartex_pentest_v1；评分与对比跑外置 veto-eval CLI
            </span>
          </CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-2">
          {exports.map((ex) => (
            <div key={ex.task_id} className="flex flex-wrap items-center gap-2 rounded-md border px-3 py-2">
              <span className="font-medium">任务 {ex.task_id}</span>
              <Badge variant={ex.response?.sod_chain_valid ? "default" : "destructive"}>
                {ex.response?.sod_chain_valid ? "SoD 链有效" : "SoD 链异常"}
              </Badge>
              <Badge variant="outline">确认类别 {(ex.response?.confirmed_vuln_classes ?? []).length}</Badge>
              <Badge variant="outline">复测确认 {ex.response?.retest_confirmed_count ?? 0}</Badge>
              <Badge variant={(ex.response?.guard_violations ?? 0) > 0 ? "secondary" : "outline"}>
                拦截 {ex.response?.guard_violations ?? 0}
              </Badge>
              <Badge variant="outline">工具 {ex.response?.tool_calls ?? 0}</Badge>
              <Badge variant="outline">tokens {ex.response?.total_tokens ?? 0}</Badge>
              <span className="text-muted-foreground ml-auto text-xs">{fmtTime(ex.exported_at)}</span>
            </div>
          ))}
          {exports.length === 0 && (
            <p className="text-muted-foreground text-sm">暂无导出（任务到终态后自动落 data/veto/&lt;任务号&gt;/）</p>
          )}
        </CardContent>
      </Card>

      {/* ④ 启动指引 */}
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <GaugeIcon className="size-4 text-primary" /> 从哪里启动
          </CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-3 text-sm">
          <div className="rounded-md border p-3">
            <p className="font-medium">① 起 Amber 服务（外置记忆库）</p>
            <pre className="bg-muted mt-1 overflow-auto rounded-md px-3 py-2 font-mono text-xs">{`cd amber-memory && pip install -e . && amber-server   # 或 uvicorn amber.server:create_app --factory
# 鉴权（可选但建议）：AMBER_REQUIRE_API_AUTH=1 + AMBER_OWNER_TOKEN/AMBER_WORKER_TOKEN
# reArtex 侧：本页打开「启用」+ 服务地址（或 通知推送 → 外置记忆库 卡片）`}</pre>
          </div>
          <div className="rounded-md border p-3">
            <p className="font-medium">② agent 测评器（Veto）评分</p>
            <pre className="bg-muted mt-1 overflow-auto rounded-md px-3 py-2 font-mono text-xs">{`# 任务终态后回执已自动导出（本页可查）；评分在 veto-eval 仓：
cd veto-eval && python -m veto.eval_core score data/veto/<任务号>/responses.json
# 基线对比：python -m veto.eval_core compare baseline.json candidate.json`}</pre>
          </div>
          <div className="rounded-md border p-3">
            <p className="font-medium">③ 自动改进闭环（J3）——推迟中</p>
            <p className="text-muted-foreground mt-1 text-xs">
              curator 现处观察态（只记录不落写）；自动晋升/自动调优需 E2 基线录制 + J2 观察数据齐备后开启，
              到位前本页的所有面都是只读——这是有意的安全裁决，不是缺失。
            </p>
          </div>
          <a
            className="text-primary inline-flex items-center gap-1 text-xs"
            href="https://github.com/Autumn-27/artex/tree/main/amber-memory"
            target="_blank"
            rel="noreferrer"
          >
            <ExternalLinkIcon className="size-3" /> amber-memory 部署手册（仓内 README）
          </a>
        </CardContent>
      </Card>
    </div>
  );
}

// MemoryRow 是记忆条目的统一渲染（正式/隔离/检索命中三处复用）。
function MemoryRow({ item, quarantined }: { item: AmberMemoryItem; quarantined?: boolean }) {
  return (
    <div className="rounded-md border px-3 py-2">
      <div className="flex flex-wrap items-center gap-2">
        <Badge variant={quarantined ? "destructive" : "outline"}>
          {quarantined ? "隔离区" : (item.state ?? "active")}
        </Badge>
        <span className="text-muted-foreground font-mono text-xs">{item.scope ?? "—"}</span>
        <span className="text-muted-foreground ml-auto text-xs">{fmtTime(item.created)}</span>
      </div>
      <p className="mt-1 line-clamp-4 text-xs whitespace-pre-wrap">{item.body ?? "—"}</p>
    </div>
  );
}
