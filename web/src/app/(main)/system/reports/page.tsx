"use client";

import * as React from "react";

import { DownloadIcon, FileTextIcon, PencilIcon, PlusIcon, Trash2Icon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type { ReportCandidate, ReportFull, ReportMeta } from "@/lib/types";

// fmtBytes 报告体积的人话（列表列）。
function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(2)} MB`;
}

// fmtTime 更新时间的人话。
function fmtTime(s: string): string {
  const d = new Date(s);
  return Number.isNaN(d.getTime()) ? s : d.toLocaleString("zh-CN", { hour12: false });
}

// ReportsPage 是报告中心：最终报告统一管理——点击直接展示、实时编辑、下载导出。
// 正文存库（不占工作空间）；下载从库出（与工作空间下载同体验，.md 附件）。
export default function ReportsPage() {
  const [reports, setReports] = React.useState<ReportMeta[]>([]);
  const [loaded, setLoaded] = React.useState(false);

  // 编辑器状态：打开时持有整篇 + 标题/标签草稿。
  const [editing, setEditing] = React.useState<ReportFull | null>(null);
  const [draftTitle, setDraftTitle] = React.useState("");
  const [draftTags, setDraftTags] = React.useState("");
  const [draftContent, setDraftContent] = React.useState("");
  const [saving, setSaving] = React.useState(false);

  // 导入状态。
  const [importOpen, setImportOpen] = React.useState(false);
  const [candidates, setCandidates] = React.useState<ReportCandidate[]>([]);
  const [importLoaded, setImportLoaded] = React.useState(false);
  const [importing, setImporting] = React.useState("");

  // 从任务生成（0.14.5 批次四）：确定性拼装初稿，终审人工。
  const [genTaskID, setGenTaskID] = React.useState("");
  const [genBusy, setGenBusy] = React.useState(false);

  const doGenerate = async () => {
    if (!genTaskID.trim()) return;
    setGenBusy(true);
    try {
      const r = await api.reportGenerate(Number(genTaskID));
      toast.success(`初稿已生成：「${r.title}」（报告中心可编辑）`);
      setGenTaskID("");
      load();
    } catch (e) {
      toast.error(`生成失败：${(e as Error).message}`);
    } finally {
      setGenBusy(false);
    }
  };

  // 标签过滤（0.15.43 #8）：reartex:report 为系统标签（生成初稿自动打标，
  // forward-only——人工编辑不可移除）。
  const [tagFilter, setTagFilter] = React.useState("");
  const load = React.useCallback(() => {
    api
      .reports(tagFilter.trim() || undefined)
      .then(setReports)
      .catch((e) => toast.error(`读取报告列表失败：${(e as Error).message}`))
      .finally(() => setLoaded(true));
  }, [tagFilter]);

  React.useEffect(() => {
    load();
  }, [load]);

  function openEditor(full: ReportFull) {
    setEditing(full);
    setDraftTitle(full.title);
    setDraftTags(full.tags);
    setDraftContent(full.content);
  }

  function openRow(r: ReportMeta) {
    api
      .reportGet(r.id)
      .then(openEditor)
      .catch((e) => toast.error((e as Error).message));
  }

  function openNew() {
    const title = window.prompt("报告标题：", "未命名报告");
    if (!title?.trim()) return;
    api
      .createReport(title.trim())
      .then((r) => {
        toast.success("已创建");
        load();
        return api.reportGet(r.id).then(openEditor);
      })
      .catch((e) => toast.error(`创建失败：${(e as Error).message}`));
  }

  function saveEdit() {
    if (!editing) return;
    setSaving(true);
    api
      .updateReport(editing.id, { title: draftTitle, content: draftContent, tags: draftTags })
      .then(() => {
        toast.success("已保存");
        setEditing(null);
        load();
      })
      .catch((e) => toast.error(`保存失败：${(e as Error).message}`))
      .finally(() => setSaving(false));
  }

  function download(r: ReportMeta) {
    api.reportDownload(r.id, r.title).catch((e) => toast.error((e as Error).message));
  }

  function remove(r: ReportMeta) {
    if (!window.confirm(`删除报告「${r.title}」？工作空间里的原始文件不受影响。`)) return;
    api
      .deleteReport(r.id)
      .then(() => {
        toast.success("已删除");
        load();
      })
      .catch((e) => toast.error(`删除失败：${(e as Error).message}`));
  }

  function openImport() {
    setImportOpen(true);
    setImportLoaded(false);
    api
      .reportsDiscover()
      .then((items) => {
        setCandidates(items);
        setImportLoaded(true);
      })
      .catch((e) => {
        toast.error(`扫描工作空间失败：${(e as Error).message}`);
        setImportLoaded(true);
      });
  }

  function doImport(c: ReportCandidate) {
    if (c.is_dir || importing) return;
    setImporting(c.path);
    api
      .importReport(c.path)
      .then((r) => {
        toast.success(`已导入「${r.title}」`);
        setImportOpen(false);
        load();
        return api.reportGet(r.id).then(openEditor);
      })
      .catch((e) => toast.error(`导入失败：${(e as Error).message}`))
      .finally(() => setImporting(""));
  }

  // renderImportList 渲染导入候选（避开嵌套三元 lint：三分支各走各的 return）。
  function renderImportList(): React.ReactNode {
    if (!importLoaded) {
      return <p className="py-6 text-center text-muted-foreground text-sm">扫描中…</p>;
    }
    const files = candidates.filter((c) => !c.is_dir);
    if (files.length === 0) {
      return <p className="py-6 text-center text-muted-foreground text-sm">工作空间里没有找到可导入的报告文件。</p>;
    }
    return (
      <div className="flex flex-col gap-1.5">
        {files.map((c) => (
          <button
            key={c.path}
            type="button"
            disabled={importing === c.path}
            onClick={() => doImport(c)}
            className="flex items-center gap-2 rounded-md border px-2.5 py-1.5 text-left text-sm transition hover:bg-muted/50 disabled:opacity-50"
          >
            <FileTextIcon className="size-4 shrink-0 text-muted-foreground" />
            <span className="min-w-0 flex-1 truncate font-mono text-xs">{c.path}</span>
            <span className="shrink-0 text-muted-foreground text-xs tabular-nums">
              {c.size != null ? fmtBytes(c.size) : ""}
            </span>
          </button>
        ))}
      </div>
    );
  }

  // renderListBody 渲染列表（避开嵌套三元 lint：三分支各走各的 return）。
  function renderListBody(): React.ReactNode {
    if (!loaded) {
      return <p className="py-6 text-center text-muted-foreground text-sm">加载中…</p>;
    }
    if (reports.length === 0) {
      return (
        <p className="py-8 text-center text-muted-foreground text-sm">
          还没有报告——右上角「新建报告」或「从工作空间导入」把任务产出的报告收进来。
        </p>
      );
    }
    return (
      <div className="overflow-hidden rounded-md border">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>名称</TableHead>
              <TableHead className="w-24">大小</TableHead>
              <TableHead className="w-36">标签</TableHead>
              <TableHead className="hidden w-56 md:table-cell">来源</TableHead>
              <TableHead className="w-40">更新时间</TableHead>
              <TableHead className="w-32 text-right">操作</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {reports.map((r) => (
              <TableRow key={r.id} className="cursor-pointer hover:bg-muted/40" onClick={() => openRow(r)}>
                <TableCell className="font-medium">
                  <span className="flex items-center gap-2">
                    <FileTextIcon className="size-4 shrink-0 text-muted-foreground" />
                    {r.title}
                  </span>
                </TableCell>
                <TableCell className="text-muted-foreground tabular-nums">{fmtBytes(r.size)}</TableCell>
                <TableCell>
                  <span className="flex flex-wrap items-center gap-1">
                    {(r.tags ?? "")
                      .split(",")
                      .filter(Boolean)
                      .map((t) => (
                        <Badge
                          key={t}
                          variant={t === "reartex:report" ? "default" : "outline"}
                          className="font-mono text-[10px]"
                          title={t === "reartex:report" ? "系统标签（生成初稿自动打标，不可移除）" : undefined}
                        >
                          {t}
                        </Badge>
                      ))}
                  </span>
                </TableCell>
                <TableCell className="hidden md:table-cell">
                  {r.source_path ? (
                    <Badge variant="outline" className="max-w-48 truncate font-mono text-[10px]">
                      {r.source_path}
                    </Badge>
                  ) : (
                    <span className="text-muted-foreground text-xs">手工创建</span>
                  )}
                </TableCell>
                <TableCell className="text-muted-foreground text-xs tabular-nums">{fmtTime(r.updated_at)}</TableCell>
                <TableCell className="text-right">
                  {/* biome-ignore lint/a11y/noStaticElementInteractions: 行内操作区仅阻止冒泡，不做交互元素（与 system/mcp 页同款纪律） */}
                  <span
                    className="inline-flex items-center gap-1"
                    onClick={(e) => e.stopPropagation()}
                    onKeyDown={(e) => e.stopPropagation()}
                  >
                    <Button size="icon" variant="ghost" className="size-7" title="编辑" onClick={() => openRow(r)}>
                      <PencilIcon className="size-3.5" />
                    </Button>
                    <Button size="icon" variant="ghost" className="size-7" title="下载 .md" onClick={() => download(r)}>
                      <DownloadIcon className="size-3.5" />
                    </Button>
                    <Button size="icon" variant="ghost" className="size-7" title="删除" onClick={() => remove(r)}>
                      <Trash2Icon className="size-3.5 text-destructive" />
                    </Button>
                  </span>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
    );
  }

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">报告中心</h1>
          <p className="text-muted-foreground text-sm">
            最终报告统一管理 · 点击直接展示、实时编辑 · 下载导出为 .md（与工作空间下载同体验）
          </p>
        </div>
        <Input
          className="h-8 w-44"
          placeholder="按标签过滤（如 reartex:report）"
          value={tagFilter}
          onChange={(e) => setTagFilter(e.target.value)}
          onBlur={() => load()}
          onKeyDown={(e) => {
            if (e.key === "Enter") load();
          }}
        />
        <div className="flex gap-2">
          {/* 从任务生成（0.14.5 批次四）：确定性拼装初稿，终审人工 */}
          <div className="flex items-center gap-1.5 rounded-md border px-2">
            <input
              className="h-8 w-28 bg-transparent text-sm outline-none"
              placeholder="任务 id"
              value={genTaskID}
              onChange={(e) => setGenTaskID(e.target.value)}
            />
            <Button
              variant="ghost"
              size="sm"
              className="h-7 text-xs"
              disabled={!genTaskID.trim() || genBusy}
              onClick={doGenerate}
            >
              {genBusy ? "生成中…" : "从任务生成初稿"}
            </Button>
          </div>
          <Button variant="outline" onClick={openImport}>
            <FileTextIcon /> 从工作空间导入
          </Button>
          <Button onClick={openNew}>
            <PlusIcon /> 新建报告
          </Button>
        </div>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">全部报告（{reports.length}）</CardTitle>
          <CardDescription>按更新时间倒序 · 正文整篇存库，工作空间原始文件不受影响</CardDescription>
        </CardHeader>
        <CardContent>{renderListBody()}</CardContent>
      </Card>

      {/* 编辑/查看：标题 + 正文实时编辑，保存落库 */}
      <Sheet open={!!editing} onOpenChange={(o) => !o && setEditing(null)}>
        <SheetContent side="right" className="flex w-full flex-col gap-0 data-[side=right]:sm:max-w-3xl">
          <SheetHeader>
            <SheetTitle className="flex items-center gap-2">
              <FileTextIcon className="size-4" /> {editing?.source_path ? "查看 / 编辑报告" : "编辑报告"}
            </SheetTitle>
            <SheetDescription>实时编辑，保存即落库；下载导出为 .md（与工作空间下载同体验）</SheetDescription>
          </SheetHeader>
          <div className="flex min-h-0 flex-1 flex-col gap-3 p-4">
            <div className="grid gap-2">
              <Label htmlFor="report-title">标题</Label>
              <Input id="report-title" value={draftTitle} onChange={(e) => setDraftTitle(e.target.value)} />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="report-tags">标签（逗号分隔）</Label>
              <Input
                id="report-tags"
                value={draftTags}
                onChange={(e) => setDraftTags(e.target.value)}
                placeholder="reartex:report 为系统标签，保存时自动保留"
              />
            </div>
            <div className="grid min-h-0 flex-1 gap-2">
              <Label htmlFor="report-content">正文（Markdown）</Label>
              <Textarea
                id="report-content"
                className="min-h-0 flex-1 resize-none font-mono text-sm leading-relaxed"
                value={draftContent}
                onChange={(e) => setDraftContent(e.target.value)}
              />
            </div>
            <div className="flex items-center justify-between">
              <span className="text-muted-foreground text-xs tabular-nums">{draftContent.length} 字符</span>
              <div className="flex gap-2">
                {editing && (
                  <Button variant="outline" onClick={() => api.reportDownload(editing.id, draftTitle || editing.title)}>
                    <DownloadIcon /> 下载 .md
                  </Button>
                )}
                <Button onClick={saveEdit} disabled={saving || !draftTitle.trim()}>
                  {saving ? "保存中…" : "保存"}
                </Button>
              </div>
            </div>
          </div>
        </SheetContent>
      </Sheet>

      {/* 从工作空间导入：扫描 *.md/*.txt/*.html 候选，点文件即导入 */}
      <Sheet open={importOpen} onOpenChange={setImportOpen}>
        <SheetContent side="right" className="flex w-full flex-col gap-0 data-[side=right]:sm:max-w-xl">
          <SheetHeader>
            <SheetTitle>从工作空间导入报告</SheetTitle>
            <SheetDescription>
              扫描工作空间里的 .md / .txt / .html——点文件即拷贝正文入库（原文件留在原地不动）
            </SheetDescription>
          </SheetHeader>
          <div className="min-h-0 flex-1 overflow-y-auto p-4">{renderImportList()}</div>
        </SheetContent>
      </Sheet>
    </div>
  );
}
