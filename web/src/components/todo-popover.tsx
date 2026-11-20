"use client";

import * as React from "react";

import { ListTodo } from "lucide-react";

import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { cn } from "@/lib/utils";

// TodoPopover shows the latest TodoWrite state of a session (chat conversation or
// task worker/planner replay). The full todo list isn't in the activity list
// (detail is lazy), so on open it fetches the detail of the most-recent TodoWrite
// call — by seq — and parses its {todos:[…]} JSON. Purely client-side; the caller
// supplies the seq + a detail fetcher (conversation or exploration endpoint).
//
// 0.15.23：mounted in the session header for ALL session kinds (worker included,
// which previously had no entry at all), refreshes while open every few seconds
// so a running worker's progress stays live, and degrades to an explanatory
// panel instead of a dead disabled button when the session never called
// TodoWrite.
const TODO_REFRESH_MS = 5000;

export function TodoPopover({
  seq,
  fetchDetail,
}: {
  seq: number | null;
  fetchDetail: (seq: number) => Promise<string>;
}) {
  const [open, setOpen] = React.useState(false);
  const [todos, setTodos] = React.useState<{ content: string; status: string }[] | null>(null);
  const [loading, setLoading] = React.useState(false);
  const [err, setErr] = React.useState("");

  const load = React.useCallback(async () => {
    if (seq == null) {
      setTodos(null);
      return;
    }
    setLoading(true);
    setErr("");
    try {
      const detail = await fetchDetail(seq);
      const start = detail.indexOf("{"); // detail 可能带 "TodoWrite " 前缀
      const parsed = JSON.parse(start >= 0 ? detail.slice(start) : detail);
      setTodos(Array.isArray(parsed?.todos) ? parsed.todos : []);
    } catch {
      setErr("解析 Todo 失败");
      setTodos(null);
    } finally {
      setLoading(false);
    }
  }, [seq, fetchDetail]);

  // refetch on each open — todos change as the run progresses — and keep
  // refreshing while open so a live run's list stays current without manual
  // reopen. Closed ⇒ timer torn down.
  React.useEffect(() => {
    if (!open) return;
    void load();
    const t = setInterval(() => void load(), TODO_REFRESH_MS);
    return () => clearInterval(t);
  }, [open, load]);

  const done = (todos ?? []).filter((t) => t.status === "completed").length;
  const MARK: Record<string, string> = { pending: "☐", in_progress: "▶", completed: "✔" };
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <button
          type="button"
          title={seq == null ? "本会话还没有 TodoWrite 记录" : "查看最近 Todo"}
          className="flex items-center gap-0.5 text-xs opacity-70 transition-colors hover:text-primary hover:opacity-100"
        >
          <ListTodo className="size-3" />
          Todo
        </button>
      </PopoverTrigger>
      <PopoverContent align="end" className="max-h-80 w-80 overflow-auto p-2">
        <p className="px-1 pb-1 font-medium text-[11px] text-muted-foreground">
          最近 Todo
          {seq != null && todos && !loading && ` · ${done}/${todos.length} 完成`}
          {loading ? " · 加载中…" : ""}
        </p>
        {seq == null && !loading && (
          <p className="px-1 text-muted-foreground text-xs">
            该会话没有用 TodoWrite 记录过进度（会话内的待办清单由 agent 自行记录）。
          </p>
        )}
        {err && <p className="px-1 text-destructive text-xs">{err}</p>}
        {todos && todos.length === 0 && !loading && <p className="px-1 text-muted-foreground text-xs">（空）</p>}
        <ul className="space-y-0.5">
          {(todos ?? []).map((t, i) => (
            <li
              key={`${i}:${t.content}`}
              className={cn(
                "flex gap-1.5 px-1 text-xs",
                t.status === "completed" && "text-muted-foreground line-through",
              )}
            >
              <span className="shrink-0">{MARK[t.status] ?? "☐"}</span>
              <span className="break-words">{t.content}</span>
            </li>
          ))}
        </ul>
      </PopoverContent>
    </Popover>
  );
}
