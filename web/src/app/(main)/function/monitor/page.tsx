// B-09H：Monitor 仓库监测页面（仓库配置 + 候选发现 + 三种显式人工操作）。
//
// 纪律（卡明文 + 压缩包安全原则）：
//   - 情报纪律：Monitor 判定只是情报候选（上游提交疑似安全更新），绝不等于
//     「漏洞已确认」——页面文案与徽章如实呈现；复现只创建待验证任务（服务端
//     B-09G 语义），本页绝不自动执行任何测试或利用；
//   - pending_review 明确突出（待人工审阅，模型未判定）——绝不把未判定渲染成
//     安全候选或已忽略（卡要求 3 逐字）；
//   - 忽略必须填写原因（空理由禁用提交）；复现与重判二次确认并展示目标
//     commit（卡要求 4 逐字）；操作结果如实反馈，失败绝不显示成功；
//   - 不显示任何 token/auth_ref——仓库行只显示「凭据引用已配置/匿名」布尔态
//     （auth_ref 引用值在响应里存在但本页结构性不渲染）；repo_url 纯文本展示，
//     不做链接（本页不提供任意 URL 直连浏览，卡要求 5 逐字）；
//   - 请求复用既有 http() 客户端（Bearer 鉴权 + 401 登出跳转），不另造请求/
//     鉴权通道，不引入另一套状态管理（卡要求 1 逐字）；
//   - 外部响应不可信：逐字段防御式收敛（缺字段/类型不符按安全默认），mock
//     模式未命中路径回 {} 时按合法空态处理，绝不崩溃；
//   - 边界如实：findings 服务端单页上限 500（B-09G 契约），状态过滤在本页做
//     （一次全取 500 再过滤，减少请求次数）；仓库/findings 列表均为只读拉取
//     +手动刷新，无轮询。
"use client";

import * as React from "react";

import {
  AlertCircleIcon,
  EyeIcon,
  GitCommitHorizontalIcon,
  Loader2Icon,
  RefreshCwIcon,
  SearchXIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import { ApiError, http } from "@/lib/api";
import { cn } from "@/lib/utils";

// ── 契约类型（与 server/monitor_repos.go / server/monitor_findings.go 对齐）──

type MonitorRepoView = {
  id: number;
  repo_url: string;
  branch: string;
  enabled: boolean;
  has_auth_ref: boolean; // 由 auth_ref 字符串收敛成布尔——引用值绝不进渲染
  last_seen_sha: string;
  created_at: string;
};

type MonitorFindingView = {
  id: number;
  repo_id: number;
  repo_url: string;
  branch: string;
  sha: string;
  title: string;
  cve_id: string;
  ghsa_id: string;
  status: string; // new | pending_review | reproduced | ignored
  created_at: string;
};

// ── 防御式收敛：未知/残缺形态一律安全默认（外部响应是不可信数据）──────────────

function normalizeRepo(raw: unknown): MonitorRepoView | null {
  const r = (raw ?? {}) as Partial<MonitorRepoView> & { auth_ref?: unknown };
  if (typeof r.id !== "number" || typeof r.repo_url !== "string" || !r.repo_url) return null;
  return {
    id: r.id,
    repo_url: r.repo_url,
    branch: typeof r.branch === "string" ? r.branch : "",
    enabled: r.enabled === true,
    has_auth_ref: typeof r.auth_ref === "string" && r.auth_ref.length > 0,
    last_seen_sha: typeof r.last_seen_sha === "string" ? r.last_seen_sha : "",
    created_at: typeof r.created_at === "string" ? r.created_at : "",
  };
}

function normalizeFinding(raw: unknown): MonitorFindingView | null {
  const f = (raw ?? {}) as Partial<MonitorFindingView>;
  if (typeof f.id !== "number" || typeof f.sha !== "string" || !f.sha) return null;
  return {
    id: f.id,
    repo_id: typeof f.repo_id === "number" ? f.repo_id : 0,
    repo_url: typeof f.repo_url === "string" ? f.repo_url : "",
    branch: typeof f.branch === "string" ? f.branch : "",
    sha: f.sha,
    title: typeof f.title === "string" ? f.title : "",
    cve_id: typeof f.cve_id === "string" ? f.cve_id : "",
    ghsa_id: typeof f.ghsa_id === "string" ? f.ghsa_id : "",
    status: typeof f.status === "string" ? f.status : "",
    created_at: typeof f.created_at === "string" ? f.created_at : "",
  };
}

// ── 状态徽章：pending_review 明确突出，四态绝不互相冒充（卡要求 3）────────────

const STATUS_META: Record<string, { label: string; className: string; actionable: boolean }> = {
  new: {
    label: "安全更新候选",
    className: "border-emerald-500/50 bg-emerald-500/10 text-emerald-600",
    actionable: true,
  },
  pending_review: {
    label: "待人工审阅（模型未判定）",
    className: "border-amber-500/60 bg-amber-500/15 text-amber-700 font-medium",
    actionable: true,
  },
  reproduced: { label: "复现已发起", className: "", actionable: false },
  ignored: { label: "已忽略", className: "opacity-70", actionable: false },
};

function statusBadge(status: string) {
  const meta = STATUS_META[status];
  if (!meta) return <Badge variant="outline">{status || "未知"}</Badge>;
  return (
    <Badge variant="outline" className={meta.className || undefined}>
      {meta.label}
    </Badge>
  );
}

function fmtTime(raw: string): string {
  if (!raw) return "—";
  const t = new Date(raw);
  if (Number.isNaN(t.getTime())) return raw; // 解析不了就原样透传，绝不编造时间
  return t.toLocaleString();
}

function shortSha(sha: string): string {
  return sha.length > 10 ? sha.slice(0, 10) : sha;
}

const FINDING_FILTERS: { value: string; label: string }[] = [
  { value: "", label: "全部" },
  { value: "new", label: "候选" },
  { value: "pending_review", label: "待审" },
  { value: "reproduced", label: "复现已发起" },
  { value: "ignored", label: "已忽略" },
];

// ── 处置弹窗目标（复现/重判共用确认弹窗；忽略用独立理由弹窗）──────────────────

type ConfirmTarget = { kind: "reproduce" | "rejudge"; finding: MonitorFindingView };

// FilteredFindingsTable 是发现表格（仅展示过滤后的行；操作回调上抛给页面——
// 弹窗状态留在页面层）。已处置态（reproduced/ignored）不提供操作入口（服务端
// B-09G 也会 409 拒绝——两层一致）。
function FilteredFindingsTable(props: {
  visibleFindings: MonitorFindingView[];
  totalCount: number;
  submitting: boolean;
  onReproduce: (f: MonitorFindingView) => void;
  onRejudge: (f: MonitorFindingView) => void;
  onIgnore: (f: MonitorFindingView) => void;
}) {
  const { visibleFindings, totalCount, submitting, onReproduce, onRejudge, onIgnore } = props;
  return (
    <div>
      <div className="overflow-x-auto">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>仓库 / 分支</TableHead>
              <TableHead>提交</TableHead>
              <TableHead>标题 / 判定</TableHead>
              <TableHead>关联编号</TableHead>
              <TableHead>状态</TableHead>
              <TableHead>发现时间</TableHead>
              <TableHead className="text-right">操作</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {visibleFindings.map((f) => {
              const actionable = STATUS_META[f.status]?.actionable ?? false;
              return (
                <TableRow key={f.id}>
                  <TableCell className="max-w-48 truncate text-sm" title={f.repo_url}>
                    {f.repo_url || `仓库 #${f.repo_id}`}
                    {f.branch && <span className="block text-muted-foreground text-xs">{f.branch}</span>}
                  </TableCell>
                  <TableCell className="font-mono text-xs">{shortSha(f.sha)}</TableCell>
                  <TableCell className="max-w-xs">
                    <div className="truncate text-sm" title={f.title}>
                      {f.title || "—"}
                    </div>
                  </TableCell>
                  <TableCell>
                    <div className="flex flex-wrap gap-1">
                      {f.cve_id && <Badge variant="destructive">{f.cve_id}</Badge>}
                      {f.ghsa_id && <Badge variant="destructive">{f.ghsa_id}</Badge>}
                      {!f.cve_id && !f.ghsa_id && <span className="text-muted-foreground text-xs">—</span>}
                    </div>
                  </TableCell>
                  <TableCell>{statusBadge(f.status)}</TableCell>
                  <TableCell className="whitespace-nowrap text-muted-foreground text-xs">
                    {fmtTime(f.created_at)}
                  </TableCell>
                  <TableCell className="text-right">
                    {actionable ? (
                      <div className="inline-flex justify-end gap-1">
                        <Button size="sm" variant="outline" disabled={submitting} onClick={() => onReproduce(f)}>
                          <EyeIcon />
                          复现
                        </Button>
                        <Button size="sm" variant="outline" disabled={submitting} onClick={() => onRejudge(f)}>
                          <GitCommitHorizontalIcon />
                          重判
                        </Button>
                        <Button size="sm" variant="outline" disabled={submitting} onClick={() => onIgnore(f)}>
                          忽略…
                        </Button>
                      </div>
                    ) : (
                      <span className="text-muted-foreground text-xs">已处置</span>
                    )}
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      </div>
      <p className="mt-3 px-4 text-muted-foreground text-xs">
        显示 {visibleFindings.length}/{totalCount} 条（服务端单页上限 500 条）· 状态过滤为本页过滤 ·
        复现=创建未启动验证任务；重判=重新调模型；忽略=需填原因并入审计链
      </p>
    </div>
  );
}

export default function MonitorPage() {
  const [repos, setRepos] = React.useState<MonitorRepoView[]>([]);
  const [findings, setFindings] = React.useState<MonitorFindingView[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);
  const [statusFilter, setStatusFilter] = React.useState("");
  // 处置弹窗状态：确认目标（复现/重判）、忽略目标+理由、提交中（防重复点击）、
  // 上次操作错误（网络失败如实渲染，绝不显示成功）。
  const [confirmTarget, setConfirmTarget] = React.useState<ConfirmTarget | null>(null);
  const [ignoreTarget, setIgnoreTarget] = React.useState<MonitorFindingView | null>(null);
  const [ignoreReason, setIgnoreReason] = React.useState("");
  const [submitting, setSubmitting] = React.useState(false);
  // seqRef 丢弃过期响应：连续刷新时只有最新一次请求能落状态。
  const seqRef = React.useRef(0);

  const load = React.useCallback(async () => {
    const seq = ++seqRef.current;
    setLoading(true);
    setError(null);
    try {
      // 仓库与发现并行拉取；findings limit=500 是 B-09G 服务端单页上限。
      const [rawRepos, rawFindings] = await Promise.all([
        http<unknown>("/monitor/repos"),
        http<unknown>("/monitor/findings?limit=500"),
      ]);
      if (seq !== seqRef.current) return;
      const repoList = Array.isArray((rawRepos as { repos?: unknown[] }).repos)
        ? (rawRepos as { repos: unknown[] }).repos
        : [];
      const reposNorm = repoList.map(normalizeRepo).filter((v): v is MonitorRepoView => v !== null);
      const repoByID = new Map(reposNorm.map((r) => [r.id, r]));
      const findingList = Array.isArray((rawFindings as { findings?: unknown[] }).findings)
        ? (rawFindings as { findings: unknown[] }).findings
        : [];
      const findingsNorm = findingList
        .map(normalizeFinding)
        .filter((v): v is MonitorFindingView => v !== null)
        .map((f) => {
          const r = repoByID.get(f.repo_id);
          return { ...f, repo_url: f.repo_url || r?.repo_url || "", branch: f.branch || r?.branch || "" };
        });
      setRepos(reposNorm);
      setFindings(findingsNorm);
    } catch (e) {
      if (seq !== seqRef.current) return;
      setError(
        e instanceof ApiError ? `加载失败（HTTP ${e.status}）：${e.message}` : `加载失败：${(e as Error).message}`,
      );
    } finally {
      if (seq === seqRef.current) setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  const visibleFindings = React.useMemo(
    () => (statusFilter ? findings.filter((f) => f.status === statusFilter) : findings),
    [findings, statusFilter],
  );

  // ── 三种显式人工操作（唯一写面；结果如实反馈，失败绝不显示成功）────────────

  const runAction = async (target: ConfirmTarget) => {
    if (submitting) return;
    const path = target.kind === "reproduce" ? "reproduce" : "rejudge";
    setSubmitting(true);
    try {
      const raw = await http<{ outcome?: unknown; reason?: unknown }>(
        `/monitor/findings/${target.finding.id}/${path}`,
        { method: "POST" },
      );
      if (target.kind === "reproduce") {
        toast.success(`已创建验证任务（发现 #${target.finding.id}）——任务未启动，请在任务页配置范围后人工执行`);
      } else {
        const outcome = typeof raw.outcome === "string" ? raw.outcome : "";
        if (outcome === "new")
          toast.success(`重判完成：仍是安全更新候选（${typeof raw.reason === "string" ? raw.reason : ""}）`);
        else if (outcome === "not_security") toast.info(`重判完成：模型认为非安全更新——已保留记录，可忽略处置`);
        else toast.warning(`重判未决：${typeof raw.reason === "string" ? raw.reason : "已转人工待审"}`);
      }
      setConfirmTarget(null);
      await load();
    } catch (e) {
      const msg =
        e instanceof ApiError ? `操作失败（HTTP ${e.status}）：${e.message}` : `操作失败：${(e as Error).message}`;
      toast.error(msg);
    } finally {
      setSubmitting(false);
    }
  };

  const runIgnore = async () => {
    if (!ignoreTarget || submitting) return;
    const reason = ignoreReason.trim();
    if (!reason) return; // 理由必填（提交按钮已禁用，此处兜底）
    setSubmitting(true);
    try {
      await http<unknown>(`/monitor/findings/${ignoreTarget.id}/ignore`, {
        method: "POST",
        body: JSON.stringify({ reason }),
      });
      toast.success(`已忽略发现 #${ignoreTarget.id}（原因已入审计链）`);
      setIgnoreTarget(null);
      setIgnoreReason("");
      await load();
    } catch (e) {
      const msg =
        e instanceof ApiError ? `忽略失败（HTTP ${e.status}）：${e.message}` : `忽略失败：${(e as Error).message}`;
      toast.error(msg);
    } finally {
      setSubmitting(false);
    }
  };

  const firstLoad = loading && !error && repos.length === 0 && findings.length === 0;

  return (
    <div className="p-4 md:p-6">
      <div className="mb-4 flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="font-semibold text-xl">仓库监测</h1>
          <p className="text-muted-foreground text-sm">
            上游仓库提交监测与模型判定候选。判定只是情报线索：复现仅创建待验证任务（不自动执行），
            验证范围与授权始终由操作者在任务面人工配置。
          </p>
        </div>
        <Button size="sm" variant="outline" onClick={() => void load()} disabled={loading}>
          {loading ? <Loader2Icon className="animate-spin" /> : <RefreshCwIcon />}
          刷新
        </Button>
      </div>

      {firstLoad && (
        <Card>
          <CardContent className="flex items-center justify-center gap-2 py-12 text-muted-foreground text-sm">
            <Loader2Icon className="animate-spin" />
            加载中…
          </CardContent>
        </Card>
      )}

      {error && (
        <Card className="border-destructive/40">
          <CardContent className="flex flex-col items-center gap-3 py-12 text-center">
            <AlertCircleIcon className="text-destructive" />
            <div className="text-sm">{error}</div>
            <Button size="sm" variant="outline" onClick={() => void load()}>
              重试
            </Button>
          </CardContent>
        </Card>
      )}

      {!error && (
        <>
          {/* ① 仓库监测配置（只读展示——配置 CRUD 属系统管理面，本页不重复提供） */}
          <Card>
            <CardHeader>
              <CardTitle className="text-base">监测仓库（{repos.length}）</CardTitle>
            </CardHeader>
            <CardContent className="px-0">
              {repos.length === 0 ? (
                <div className="flex flex-col items-center gap-2 py-8 text-center">
                  <SearchXIcon className="text-muted-foreground" />
                  <div className="text-muted-foreground text-sm">
                    暂无监测仓库配置（监控配置接口 POST /api/monitor/repos 可新增）
                  </div>
                </div>
              ) : (
                <div className="overflow-x-auto">
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead>仓库</TableHead>
                        <TableHead>分支</TableHead>
                        <TableHead>状态</TableHead>
                        <TableHead>凭据</TableHead>
                        <TableHead>游标（last_seen）</TableHead>
                        <TableHead>配置时间</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {repos.map((r) => (
                        <TableRow key={r.id}>
                          <TableCell className="max-w-xs truncate font-medium" title={r.repo_url}>
                            {r.repo_url}
                          </TableCell>
                          <TableCell className="whitespace-nowrap">{r.branch || "—"}</TableCell>
                          <TableCell>
                            {r.enabled ? (
                              <Badge variant="secondary">启用</Badge>
                            ) : (
                              <Badge variant="outline" className="opacity-70">
                                停用
                              </Badge>
                            )}
                          </TableCell>
                          {/* 卡要求 5：绝不显示 token/auth_ref 值——只显示是否已配置。 */}
                          <TableCell className="text-muted-foreground text-xs">
                            {r.has_auth_ref ? "凭据引用已配置" : "匿名访问"}
                          </TableCell>
                          <TableCell className="font-mono text-muted-foreground text-xs">
                            {r.last_seen_sha ? shortSha(r.last_seen_sha) : "尚未拉取"}
                          </TableCell>
                          <TableCell className="whitespace-nowrap text-muted-foreground text-xs">
                            {fmtTime(r.created_at)}
                          </TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                </div>
              )}
            </CardContent>
          </Card>

          {/* ② 候选发现 + 三种显式人工操作 */}
          <Card className="mt-4">
            <CardHeader className="flex flex-wrap items-center justify-between gap-2">
              <CardTitle className="text-base">监测发现（{findings.length}）</CardTitle>
              <div className="flex flex-wrap gap-1">
                {FINDING_FILTERS.map((f) => (
                  <Button
                    key={f.value}
                    size="sm"
                    variant={statusFilter === f.value ? "default" : "outline"}
                    onClick={() => setStatusFilter(f.value)}
                  >
                    {f.label}
                  </Button>
                ))}
              </div>
            </CardHeader>
            <CardContent className="px-0">
              {findings.length === 0 ? (
                <div className="flex flex-col items-center gap-2 py-8 text-center">
                  <SearchXIcon className="text-muted-foreground" />
                  <div className="text-muted-foreground text-sm">暂无监测发现（poller 判定有候选后在此展示）</div>
                </div>
              ) : (
                <FilteredFindingsTable
                  visibleFindings={visibleFindings}
                  totalCount={findings.length}
                  submitting={submitting}
                  onReproduce={(f) => setConfirmTarget({ kind: "reproduce", finding: f })}
                  onRejudge={(f) => setConfirmTarget({ kind: "rejudge", finding: f })}
                  onIgnore={(f) => {
                    setIgnoreTarget(f);
                    setIgnoreReason("");
                  }}
                />
              )}
            </CardContent>
          </Card>
        </>
      )}

      {/* 复现/重判二次确认弹窗：明确展示目标 commit（卡要求 4 逐字）。 */}
      <Dialog
        open={confirmTarget !== null}
        onOpenChange={(open) => {
          if (!open && !submitting) setConfirmTarget(null);
        }}
      >
        <DialogContent className="sm:max-w-md">
          {confirmTarget && (
            <>
              <DialogHeader>
                <DialogTitle>
                  {confirmTarget.kind === "reproduce" ? "复现（创建验证任务）" : "重判（重新调模型判定）"}
                </DialogTitle>
                <DialogDescription>
                  {confirmTarget.kind === "reproduce"
                    ? "将经由现有人工建任务入口创建一条未启动的验证任务（带仓库/提交上下文）；不会自动执行任何测试或利用，任务范围与授权由你在任务面人工配置。"
                    : "将重新拉取该提交材料并再次调用模型判定；失败会转为人工待审（pending_review），绝不静默改判。"}
                </DialogDescription>
              </DialogHeader>
              <div className="grid gap-1 rounded-md border p-3 text-sm">
                <div className="flex items-center gap-2">
                  <GitCommitHorizontalIcon className="text-muted-foreground" />
                  <span className="font-mono">{confirmTarget.finding.sha}</span>
                </div>
                <div className="truncate text-muted-foreground text-xs" title={confirmTarget.finding.title}>
                  {confirmTarget.finding.title || "—"}
                </div>
                <div className="text-muted-foreground text-xs">
                  {confirmTarget.finding.repo_url || `仓库 #${confirmTarget.finding.repo_id}`}
                  {confirmTarget.finding.branch ? ` · ${confirmTarget.finding.branch}` : ""}
                </div>
                {(confirmTarget.finding.cve_id || confirmTarget.finding.ghsa_id) && (
                  <div className="flex gap-1 pt-1">
                    {confirmTarget.finding.cve_id && (
                      <Badge variant="destructive">{confirmTarget.finding.cve_id}</Badge>
                    )}
                    {confirmTarget.finding.ghsa_id && (
                      <Badge variant="destructive">{confirmTarget.finding.ghsa_id}</Badge>
                    )}
                  </div>
                )}
              </div>
              <DialogFooter>
                <DialogClose asChild>
                  <Button variant="outline" disabled={submitting}>
                    取消
                  </Button>
                </DialogClose>
                <Button
                  onClick={() => {
                    void runAction(confirmTarget);
                  }}
                  disabled={submitting}
                >
                  {submitting && <Loader2Icon className="animate-spin" />}
                  确认{confirmTarget.kind === "reproduce" ? "创建验证任务" : "重判"}
                </Button>
              </DialogFooter>
            </>
          )}
        </DialogContent>
      </Dialog>

      {/* 忽略弹窗：原因必填（空理由禁用提交），原因写入审计链。 */}
      <Dialog
        open={ignoreTarget !== null}
        onOpenChange={(open) => {
          if (!open && !submitting) {
            setIgnoreTarget(null);
            setIgnoreReason("");
          }
        }}
      >
        <DialogContent className="sm:max-w-md">
          {ignoreTarget && (
            <>
              <DialogHeader>
                <DialogTitle>忽略监测发现</DialogTitle>
                <DialogDescription>忽略需填写原因（必填）并入审计链；原始提交与判定记录不会被删除。</DialogDescription>
              </DialogHeader>
              <div className="grid gap-2">
                <div className="flex items-center gap-2 rounded-md border p-2 text-xs">
                  <GitCommitHorizontalIcon className="text-muted-foreground" />
                  <span className="font-mono">{ignoreTarget.sha}</span>
                  <span className="truncate text-muted-foreground" title={ignoreTarget.title}>
                    {ignoreTarget.title || "—"}
                  </span>
                </div>
                <Label htmlFor="monitor-ignore-reason">忽略原因（必填）</Label>
                <Textarea
                  id="monitor-ignore-reason"
                  value={ignoreReason}
                  onChange={(e) => setIgnoreReason(e.target.value)}
                  placeholder="例如：仅文档提及 CVE，非安全更新"
                  disabled={submitting}
                  rows={3}
                />
                {!ignoreReason.trim() && <span className={cn("text-amber-600 text-xs")}>必须填写原因后才能提交。</span>}
              </div>
              <DialogFooter>
                <DialogClose asChild>
                  <Button variant="outline" disabled={submitting}>
                    取消
                  </Button>
                </DialogClose>
                <Button onClick={() => void runIgnore()} disabled={submitting || !ignoreReason.trim()}>
                  {submitting && <Loader2Icon className="animate-spin" />}
                  确认忽略
                </Button>
              </DialogFooter>
            </>
          )}
        </DialogContent>
      </Dialog>
    </div>
  );
}
