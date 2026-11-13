"use client";

// 小程序渗透页（0.12.0 §四-B8）：token 池管理 + 解包产物浏览 + 任务入口。
// 链路说明（SKILL.md 为准）：包获取（人工口子）→ 解包还原 → 运行时 → 接口发现
// → 越权矩阵（Burp 重放，任务路径）。本页是配置与产物面；执行在对话与任务里。

import * as React from "react";

import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { api } from "@/lib/api";
import type { MiniprogramArtifact, MiniprogramToken } from "@/lib/types";

export default function MiniprogramPage() {
  const [taskId, setTaskId] = React.useState<string>("");
  const [tokens, setTokens] = React.useState<MiniprogramToken[]>([]);
  const [artifacts, setArtifacts] = React.useState<MiniprogramArtifact[]>([]);
  const [loading, setLoading] = React.useState(false);
  const [showAdd, setShowAdd] = React.useState(false);
  const [form, setForm] = React.useState({ app_id: "", label: "", value: "" });
  const [runtimeUrl, setRuntimeUrl] = React.useState("");
  const [runtimeInput, setRuntimeInput] = React.useState("");
  const [runtimeDirty, setRuntimeDirty] = React.useState(false);

  const load = React.useCallback(() => {
    const id = taskId.trim();
    if (!id) {
      setTokens([]);
      return;
    }
    setLoading(true);
    Promise.all([
      api.mpTokens(Number(id)).catch(() => ({ tokens: [] })),
      api.mpArtifacts().catch(() => ({ apps: [] })),
      api.mpRuntime().catch(() => ({ url: "" })),
    ])
      .then(([ts, arts, rt]) => {
        setTokens(ts.tokens ?? []);
        setArtifacts(arts.apps ?? []);
        setRuntimeUrl(rt.url ?? "");
        setRuntimeInput(rt.url ?? "");
        setRuntimeDirty(false);
      })
      .finally(() => setLoading(false))
      .catch(() => setLoading(false)); // 各请求自带 catch；这里兜 finally 链本身
  }, [taskId]);

  React.useEffect(() => {
    load();
  }, [load]);

  const addToken = async () => {
    if (!taskId.trim() || !form.app_id || !form.label || !form.value) {
      toast.error("任务 id / AppID / 角色标签 / 凭据值 均必填");
      return;
    }
    try {
      await api.mpTokenSave({ task_id: Number(taskId), ...form });
      toast.success(`token 已保存（${form.label}）——落库即加密，此处只存指纹`);
      setForm({ app_id: form.app_id, label: "", value: "" });
      load();
    } catch (err) {
      toast.error(`保存失败：${String(err)}`);
    }
  };

  const saveRuntime = async () => {
    try {
      await api.mpRuntimeSet(runtimeInput);
      toast.success(runtimeInput ? "运行时地址已设置，内嵌浏览器可用" : "运行时地址已清除");
      setRuntimeUrl(runtimeInput);
      setRuntimeDirty(false);
    } catch (err) {
      toast.error(`设置失败：${String(err)}`);
    }
  };

  const delToken = async (id: number) => {
    try {
      await api.mpTokenDelete(id);
      toast.success("token 已删除");
      load();
    } catch (err) {
      toast.error(`删除失败：${String(err)}`);
    }
  };

  return (
    <div className="flex h-full flex-col gap-4 p-4">
      <div className="flex flex-wrap items-center gap-2">
        <h1 className="font-semibold text-lg">小程序渗透</h1>
        <span className="text-muted-foreground text-xs">
          链路：包获取（人工口子）→ 解包还原 → 运行时 → 接口发现 → 越权矩阵（任务路径）。执行走「渗透测试」对话 +
          tasks；本页管理 token 池与产物。
        </span>
        <Button variant="outline" size="sm" className="ml-auto h-8 text-xs" onClick={load} disabled={loading}>
          {loading ? "加载中…" : "刷新"}
        </Button>
      </div>

      <div className="flex flex-wrap items-center gap-2">
        <input
          className="h-8 w-40 rounded-md border bg-background px-2 text-sm"
          placeholder="任务 id（加载该任务的池）"
          value={taskId}
          onChange={(e) => setTaskId(e.target.value)}
        />
        <Button variant="outline" size="sm" className="h-8 text-xs" onClick={() => setShowAdd((v) => !v)}>
          {showAdd ? "收起" : "添加 token"}
        </Button>
      </div>

      {showAdd && (
        <div className="flex flex-wrap items-end gap-2 rounded-lg border p-3">
          <label className="text-muted-foreground text-xs">
            AppID
            <input
              className="ml-1 h-8 w-36 rounded-md border bg-background px-2 text-sm"
              value={form.app_id}
              onChange={(e) => setForm({ ...form, app_id: e.target.value })}
              placeholder="wx1a2b3c4d"
            />
          </label>
          <label className="text-muted-foreground text-xs">
            角色标签
            <input
              className="ml-1 h-8 w-28 rounded-md border bg-background px-2 text-sm"
              value={form.label}
              onChange={(e) => setForm({ ...form, label: e.target.value })}
              placeholder="admin / userA / anonymous"
            />
          </label>
          <label className="text-muted-foreground text-xs">
            凭据值（落库即加密）
            <input
              className="ml-1 h-8 w-72 rounded-md border bg-background px-2 text-sm"
              type="password"
              value={form.value}
              onChange={(e) => setForm({ ...form, value: e.target.value })}
              placeholder="bearer token / session 凭据"
            />
          </label>
          <Button size="sm" className="h-8 text-xs" onClick={addToken}>
            保存
          </Button>
          <span className="text-[10px] text-muted-foreground">
            保存后此处只显示指纹；明文只在 Burp 重放的使用时刻经 mp_token_get(reveal) 取用。
          </span>
        </div>
      )}

      {/* token 池 */}
      <div className="rounded-lg border">
        <div className="border-b px-3 py-2 font-medium text-sm">token 池{taskId ? `（任务 #${taskId}）` : ""}</div>
        <div className="divide-y">
          {!taskId && (
            <div className="px-3 py-4 text-muted-foreground text-xs">输入任务 id 加载该任务的 token 池。</div>
          )}
          {taskId && tokens.length === 0 && !loading && (
            <div className="px-3 py-4 text-muted-foreground text-xs">
              该任务还没有 token——用上方表单按角色（admin/userA/userB/anonymous）录入，或在渗透对话里用 mp_token_save。
            </div>
          )}
          {tokens.map((t) => (
            <div key={t.id} className="flex flex-wrap items-center gap-2 px-3 py-2 text-xs">
              <Badge variant="outline" className="text-[10px]">
                {t.label}
              </Badge>
              <span className="text-muted-foreground">app {t.app_id}</span>
              <Badge variant="outline" className="text-[10px]">
                指纹 {t.fingerprint}
              </Badge>
              <Badge variant="outline" className="text-[10px]">
                {t.scope}
              </Badge>
              <Button
                variant="ghost"
                size="sm"
                className="ml-auto h-7 text-red-600 text-xs"
                onClick={() => delToken(t.id)}
              >
                删除
              </Button>
            </div>
          ))}
        </div>
      </div>

      {/* 内嵌浏览器：运行时反代（同源 iframe）+ 地址配置 */}
      <div className="rounded-lg border">
        <div className="flex flex-wrap items-center gap-2 border-b px-3 py-2">
          <span className="text-sm font-medium">内嵌浏览器（小程序运行时）</span>
          {runtimeUrl && (
            <Badge variant="secondary" className="text-[10px]">
              {runtimeUrl}
            </Badge>
          )}
          <div className="ml-auto flex items-center gap-1">
            <input
              className="h-8 w-56 rounded-md border bg-background px-2 text-xs"
              placeholder="运行时地址（如 http://127.0.0.1:3000）"
              value={runtimeInput}
              onChange={(e) => {
                setRuntimeInput(e.target.value);
                setRuntimeDirty(true);
              }}
            />
            <Button size="sm" className="h-8 text-xs" disabled={!runtimeDirty} onClick={saveRuntime}>
              保存
            </Button>
          </div>
        </div>
        <div className="bg-muted px-3 py-2 text-xs text-muted-foreground">
          wxa-runner 起 dev server 后把地址填到这里——小程序页面内嵌在下方（同源反代），可直接点击调试；agent 经
          chrome-devtools 操控同一页面做接口发现。运行时地址为部署配置（agent 不可改）。
        </div>
        {runtimeUrl ? (
          <iframe
            src="/mp-runtime/"
            title="小程序运行时"
            className="h-[60vh] w-full border-0"
            sandbox="allow-scripts allow-same-origin allow-forms allow-popups"
          />
        ) : (
          <div className="px-3 py-6 text-center text-xs text-muted-foreground">
            未配置运行时地址。用 wxa-runner 起 dev server（地址如 http://127.0.0.1:端口），填入上方并保存。
          </div>
        )}
      </div>

      {/* 解包产物 */}
      <div className="rounded-lg border">
        <div className="border-b px-3 py-2 font-medium text-sm">解包产物（artifacts/miniprogram/）</div>
        <div className="divide-y">
          {artifacts.length === 0 && (
            <div className="px-3 py-4 text-muted-foreground text-xs">
              还没有解包产物。跑 skills/wechat-miniprogram 的 unpack.sh（先 probe_unpackers.sh
              探针；解密密钥部分在微信进程内存——人工口子）。
            </div>
          )}
          {artifacts.map((a) => (
            <div key={a.app_id} className="px-3 py-2 text-xs">
              <div className="font-medium">{a.app_id}</div>
              <div className="text-muted-foreground">{a.files.join(" · ") || "（空目录）"}</div>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
