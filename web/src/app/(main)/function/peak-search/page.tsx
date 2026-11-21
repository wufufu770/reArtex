"use client";

import * as React from "react";

import { CheckIcon, Loader2Icon, SearchIcon, XIcon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";

// Peak Search（0.15.38）：人工专属知识蒸馏工作台。检索（URL/手动粘贴）→
// 蒸馏草稿 → 人工审批（批准才原子写 knowledge 库）。本页请求带专用 UI 头，
// agent/侧车无此头一律 403（后端守卫）。

// peakFetch 是带 UI 标识头的 fetch 封装。
async function peakFetch(path: string, init?: RequestInit) {
  const headers = new Headers(init?.headers);
  headers.set("X-Peak-Search-UI", "1");
  if (init?.body) headers.set("Content-Type", "application/json");
  const r = await fetch(path, { ...init, headers });
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error((j as { error?: string }).error ?? `HTTP ${r.status}`);
  return j;
}

type Proposal = {
  id: string;
  query: string;
  pathway: string;
  created_at: string;
  draft: { title: string; body: string; repo: string };
};

export default function PeakSearchPage() {
  const [query, setQuery] = React.useState("");
  const [text, setText] = React.useState("");
  const [repo, setRepo] = React.useState<"strategy" | "legacy">("strategy");
  const [searching, setSearching] = React.useState(false);
  const [proposals, setProposals] = React.useState<Proposal[]>([]);
  // edit_approve（修改后批准）的编辑缓冲：单条展开，一份即可。
  const [editing, setEditing] = React.useState<string | null>(null);
  const [editTitle, setEditTitle] = React.useState("");
  const [editBody, setEditBody] = React.useState("");

  const load = React.useCallback(async () => {
    try {
      const r = (await peakFetch("/api/peak-search/proposals")) as { proposals: Proposal[] };
      setProposals(r.proposals ?? []);
    } catch (e) {
      toast.error(`提议加载失败：${(e as Error).message}`);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  const search = async () => {
    if (!query.trim() && !text.trim()) return;
    setSearching(true);
    try {
      await peakFetch("/api/peak-search/search", {
        method: "POST",
        body: JSON.stringify({ query: query.trim(), text: text.trim(), repo }),
      });
      toast.success("检索完成，草稿已入待审队列");
      await load();
    } catch (e) {
      toast.error(`检索失败：${(e as Error).message}`);
    } finally {
      setSearching(false);
    }
  };

  const decide = async (id: string, action: "approve" | "reject") => {
    try {
      await peakFetch(`/api/peak-search/proposals/${id}/decide`, {
        method: "POST",
        body: JSON.stringify({ action }),
      });
      toast.success(action === "approve" ? "已批准并写入知识库" : "已丢弃");
      await load();
    } catch (e) {
      toast.error(`裁决失败：${(e as Error).message}`);
    }
  };

  // decideEdit：edit_approve——后端以提交的 title/body 覆盖草稿后原子写。
  const decideEdit = async (id: string) => {
    try {
      await peakFetch(`/api/peak-search/proposals/${id}/decide`, {
        method: "POST",
        body: JSON.stringify({ action: "edit_approve", title: editTitle, body: editBody, repo }),
      });
      toast.success("修改已保存并写入知识库");
      setEditing(null);
      await load();
    } catch (e) {
      toast.error(`裁决失败：${(e as Error).message}`);
    }
  };

  return (
    <div className="mx-auto flex w-full max-w-5xl flex-col gap-4 p-4">
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <SearchIcon className="size-4 text-primary" /> Peak Search · 知识蒸馏
            <span className="text-muted-foreground text-xs font-normal">
              人工专属（agent 无权限）；批准才写入策略/传承库
            </span>
          </CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          <div className="flex flex-wrap items-center gap-3">
            <Input
              className="min-w-64 flex-1"
              placeholder="URL 或主题关键词"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
            />
            <RadioGroup
              value={repo}
              onValueChange={(v) => setRepo(v as typeof repo)}
              className="flex items-center gap-3"
            >
              <label className="flex items-center gap-1.5 text-sm">
                <RadioGroupItem value="strategy" /> 策略库
              </label>
              <label className="flex items-center gap-1.5 text-sm">
                <RadioGroupItem value="legacy" /> 传承库
              </label>
            </RadioGroup>
            <Button onClick={search} disabled={searching || (!query.trim() && !text.trim())}>
              {searching ? <Loader2Icon className="animate-spin" /> : <SearchIcon />} 检索并蒸馏
            </Button>
          </div>
          <Textarea
            className="min-h-24"
            placeholder="（可选）手动粘贴文章正文——离线兜底路径，粘贴后直接蒸馏"
            value={text}
            onChange={(e) => setText(e.target.value)}
          />
        </CardContent>
      </Card>

      {proposals.map((p) => {
        const editable = editing === p.id;
        return (
          <Card key={p.id}>
            <CardHeader>
              <CardTitle className="flex flex-wrap items-center gap-2 text-sm">
                <Badge variant="outline">{p.pathway}</Badge>
                <Badge variant="outline">{p.draft.repo === "legacy" ? "传承库" : "策略库"}</Badge>
                <span className="min-w-0 truncate font-medium">{p.draft.title}</span>
                <span className="text-muted-foreground text-xs">{p.created_at}</span>
              </CardTitle>
            </CardHeader>
            <CardContent className="flex flex-col gap-3">
              {editable ? (
                <>
                  <Input value={editTitle} onChange={(e) => setEditTitle(e.target.value)} placeholder="标题" />
                  <Textarea
                    className="min-h-40 font-mono"
                    value={editBody}
                    onChange={(e) => setEditBody(e.target.value)}
                  />
                  <div className="flex items-center gap-2">
                    <Button size="sm" onClick={() => void decideEdit(p.id)} disabled={!editBody.trim()}>
                      <CheckIcon /> 保存修改并批准
                    </Button>
                    <Button size="sm" variant="outline" onClick={() => setEditing(null)}>
                      取消
                    </Button>
                  </div>
                </>
              ) : (
                <>
                  <pre className="max-h-64 overflow-auto rounded-md bg-muted px-3 py-2 font-mono text-xs whitespace-pre-wrap">
                    {p.draft.body}
                  </pre>
                  <div className="flex items-center gap-2">
                    <Button size="sm" onClick={() => void decide(p.id, "approve")}>
                      <CheckIcon /> 批准并写入{p.draft.repo === "legacy" ? "传承库" : "策略库"}
                    </Button>
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => {
                        setEditing(p.id);
                        setEditTitle(p.draft.title);
                        setEditBody(p.draft.body);
                      }}
                    >
                      修改后批准
                    </Button>
                    <Button size="sm" variant="outline" onClick={() => void decide(p.id, "reject")}>
                      <XIcon /> 丢弃
                    </Button>
                  </div>
                </>
              )}
            </CardContent>
          </Card>
        );
      })}
      {proposals.length === 0 && (
        <p className="text-muted-foreground px-1 text-sm">暂无待审提议。检索完成后草稿会出现在这里。</p>
      )}
    </div>
  );
}
