"use client";

import * as React from "react";

import {
  ArrowUpIcon,
  Bot,
  ChevronDownIcon,
  ChevronRightIcon,
  ListChecksIcon,
  Loader2Icon,
  MoreHorizontalIcon,
  PaperclipIcon,
  PencilIcon,
  PinIcon,
  PinOffIcon,
  PlusIcon,
  Square,
  Trash2Icon,
  XIcon,
  ZapIcon,
} from "lucide-react";
import { toast } from "sonner";

import { ApprovalExecutionFocus, useApprovalFocus, useApprovalHistory } from "@/components/approval-execution-focus";
import { MentionTextarea } from "@/components/mention-textarea";
import { CreateSessionDialog } from "@/components/session/create-session-dialog";
import {
  bindLegacyConversations,
  DEFAULT_SESSION_LIST_QUERY,
  filterSessions,
  groupSessionsByAgent,
  groupSessionsByAttachment,
  groupSessionsByKind,
  type SessionListQuery,
  type SessionSection,
  sessionMembers,
} from "@/components/session/session-format";
import { SessionList, SessionSectionHeader } from "@/components/session/session-list";
import { SessionToolbar } from "@/components/session/session-toolbar";
import { SessionView } from "@/components/session/session-view";
import { SideQuestionButton, SideQuestionWorkspace } from "@/components/side-question-workspace";
import { TodoPopover } from "@/components/todo-popover";
import { Transcript } from "@/components/transcript";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { JumpToBottom } from "@/components/jump-to-bottom";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { useSideQuestions } from "@/hooks/use-side-questions";
import { mergeActivities } from "@/lib/activity-merge";
import { ApiError, api } from "@/lib/api";
import { shouldSubmitOnKey, useChatSendMode } from "@/lib/chat-send-mode";
import { getLocalStorageValue, setLocalStorageValue } from "@/lib/local-storage.client";
import type { Session } from "@/lib/session-types";
import { isBtwCommand } from "@/lib/side-questions";
import type { Activity, Agent, ChatAttachment, Conversation, LLMProfile } from "@/lib/types";
import { cn } from "@/lib/utils";

// fmtBytes renders a human file size for attachment chips (mirrors transcript.tsx).
function fmtBytes(n: number): string {
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MB`;
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(1)} KB`;
  return `${n} B`;
}

// fmtTokens renders a compact token count (1234 → 1.2k, 2_000_000 → 2M).
function fmtTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(n >= 10_000_000 ? 0 : 1)}M`;
  if (n >= 1000) return `${(n / 1000).toFixed(n >= 10000 ? 0 : 1)}k`;
  return String(n);
}

// fmtDuration renders an elapsed milliseconds span compactly (90s → 1m30s).
function _fmtDuration(ms: number): string {
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m${String(s % 60).padStart(2, "0")}s`;
  const h = Math.floor(m / 60);
  return `${h}h${String(m % 60).padStart(2, "0")}m`;
}

// HISTORY_PAGE is how many steps one history page loads: the latest page on open,
// then one more page each time the user scrolls to the top. Kept modest so a long
// thread stays snappy (only ~a page of rows is in the DOM until you scroll up).
const HISTORY_PAGE = 200;
const CONVERSATION_LIST_PAGE = 100;

// 会话列表的轮询周期。名册的 presence 与状态点在动，所以比旧对话列表慢一点。
const SESSION_POLL_MS = 3000;
const CONVERSATION_POLL_MS = 5000;

// Which agent groups the user has collapsed in the left rail. Persisted so the
// rail looks the same after a reload; unknown keys are harmless (a deleted agent
// simply never renders a group again).
const COLLAPSED_AGENTS_KEY = "artex.chat.collapsed-agents";

// 会话列表里被收起的分区，同样存本地、刷新后保持。键 = 视图前缀 + section.key
// （sessionSectionStateKey）：分区数组随 3s 轮询整体重建，折叠态必须独立存；按 agent /
// 按工件两个视图又都有 `__none__` 分区，不加视图前缀就会互相串。「旧对话」块用固定键。
const COLLAPSED_SESSION_SECTIONS_KEY = "artex.chat.collapsed-session-sections";
const LEGACY_CONVERSATIONS_SECTION = "legacy:conversations";

function sessionSectionStateKey(view: SessionListQuery["view"], sectionKey: string): string {
  return `${view}:${sectionKey}`;
}

// 首次（本地还没有记录）默认收起的分区：群聊是数量大头、旧会话是存量；总控 / 大厅 /
// 置顶 / 今天 / 更早与「旧对话」块默认展开。点过一次之后整份折叠态写回本地，以记录为准。
const DEFAULT_COLLAPSED_SESSION_SECTIONS: readonly string[] = [
  sessionSectionStateKey("flat", "kind:chat"),
  sessionSectionStateKey("flat", "kind:legacy"),
];

function conversationIsPinned(conversation: Conversation): boolean {
  return conversation.pinned ?? Boolean(conversation.pinned_at);
}

// AgentGroup is one collapsible section of the left rail: all unpinned
// conversations of a single agent, newest activity first.
interface AgentGroup {
  key: string;
  name: string;
  conversations: Conversation[];
  runningCount: number;
}

// groupByAgent buckets conversations by agent, preserving the incoming order
// both inside a group and across groups. The server already sorts by updated_at
// DESC, so first-appearance order == most-recently-active group first.
//
// 这个函数只服务于「按 agent 分组」这个**可选视图**（方案书 §9 定稿 4：保留为
// 可选开关，默认关闭）—— 会话列表本身不再按 agent 分组。
function groupByAgent(conversations: Conversation[], agentByKey: Map<string, Agent>): AgentGroup[] {
  const groups = new Map<string, AgentGroup>();
  for (const conversation of conversations) {
    let group = groups.get(conversation.agent_key);
    if (!group) {
      group = {
        key: conversation.agent_key,
        name: agentByKey.get(conversation.agent_key)?.name || conversation.agent_key,
        conversations: [],
        runningCount: 0,
      };
      groups.set(conversation.agent_key, group);
    }
    group.conversations.push(conversation);
    if (conversation.running) group.runningCount++;
  }
  return [...groups.values()];
}

// LiveBadge is the small pulsing "实时" chip reused from the task's main-agent
// console — shown while a turn is streaming.
function LiveBadge() {
  return (
    <span className="inline-flex items-center gap-1 rounded bg-blue-500/15 px-1.5 py-0.5 font-medium text-[10px] text-blue-600 dark:text-blue-400">
      <span className="size-1 animate-pulse rounded-full bg-blue-500" />
      实时
    </span>
  );
}

// Composer is the shared bottom input (textarea grows to a cap, Enter sends,
// Shift+Enter newlines) — the same affordance across DraftChat and ChatView.
function Composer({
  value,
  onChange,
  onSend,
  disabled,
  placeholder,
  leftSlot,
  running,
  onStop,
  stopDisabled,
  attachments,
  onPickFiles,
  onRemoveAttachment,
  uploading,
  allowBtw,
}: {
  value: string;
  onChange: (v: string) => void;
  onSend: () => void;
  disabled: boolean;
  placeholder: string;
  leftSlot?: React.ReactNode;
  running?: boolean;
  onStop?: () => void;
  stopDisabled?: boolean;
  // 方式1 文件上传:传了 onPickFiles 才显示回形针按钮 + 附件 chip 预览。
  attachments?: ChatAttachment[];
  onPickFiles?: (files: File[]) => void;
  onRemoveAttachment?: (path: string) => void;
  uploading?: boolean;
  allowBtw?: boolean;
}) {
  const fileInputRef = React.useRef<HTMLInputElement>(null);
  const atts = attachments ?? [];
  // 发送键位由系统设置决定（localStorage），默认 Enter 发送。
  const sendMode = useChatSendMode();
  function onKeyDown(e: React.KeyboardEvent<HTMLTextAreaElement>) {
    if (!shouldSubmitOnKey(e, sendMode)) return;
    e.preventDefault();
    onSend();
  }
  return (
    <div className="border-t p-3">
      {atts.length > 0 && (
        <div className="mb-2 flex flex-wrap gap-1.5">
          {atts.map((a) => (
            <div
              key={a.path}
              className="flex items-center gap-1.5 rounded-md border bg-muted/50 px-2 py-1 text-xs"
              title={a.path}
            >
              <PaperclipIcon className="size-3 shrink-0 text-primary" />
              <span className="max-w-[160px] truncate">{a.name}</span>
              <span className="text-muted-foreground">{fmtBytes(a.size)}</span>
              {onRemoveAttachment && (
                <button
                  type="button"
                  className="ml-0.5 text-muted-foreground hover:text-foreground"
                  onClick={() => onRemoveAttachment(a.path)}
                  title="移除"
                >
                  <XIcon className="size-3" />
                </button>
              )}
            </div>
          ))}
        </div>
      )}
      <div className="flex flex-wrap items-center gap-2">
        {leftSlot ? <div className="w-full sm:w-auto">{leftSlot}</div> : null}
        {onPickFiles && (
          <>
            <input
              ref={fileInputRef}
              type="file"
              multiple
              className="hidden"
              onChange={(e) => {
                // FileList 与 input 元素活绑定:必须先快照成数组,再清空 value,
                // 否则异步的 onPickFiles(比如草稿态要先建会话)恢复执行时会拿到空列表。
                const picked = Array.from(e.target.files ?? []);
                e.target.value = ""; // allow re-picking the same file
                if (picked.length > 0) onPickFiles(picked);
              }}
            />
            <Button
              size="icon"
              variant="ghost"
              onClick={() => fileInputRef.current?.click()}
              disabled={disabled || uploading}
              title="上传文件"
            >
              {uploading ? <Loader2Icon className="size-4 animate-spin" /> : <PaperclipIcon className="size-4" />}
            </Button>
          </>
        )}
        <MentionTextarea
          className="max-h-40 min-h-10 min-w-0 flex-1 resize-none"
          rows={1}
          placeholder={placeholder}
          value={value}
          disabled={disabled && !(running && allowBtw)}
          onValueChange={onChange}
          onKeyDown={onKeyDown}
        />
        {running && allowBtw && isBtwCommand(value) && (
          <Button size="icon" onClick={onSend} aria-label="发送旁路问题" title="发送旁路问题">
            <ArrowUpIcon />
          </Button>
        )}
        {running ? (
          // while a run is in flight the send button becomes a stop button —
          // aborts just this session (the trigger queue keeps going).
          <Button size="icon" variant="destructive" onClick={onStop} disabled={stopDisabled} title="停止本次运行">
            <Square className="size-3.5 fill-current" />
          </Button>
        ) : (
          <Button
            size="icon"
            onClick={onSend}
            disabled={disabled || (!value.trim() && atts.length === 0)}
            title="发送消息"
            aria-label="发送消息"
          >
            <ArrowUpIcon />
          </Button>
        )}
      </div>
    </div>
  );
}

// LLMProfileRow shows the active LLM config below the composer and lets the user
// switch it via a Popover. `selected` is the profile id or null for default.
function LLMProfileRow({
  profiles,
  selected,
  onChange,
  disabled,
  rightSlot,
}: {
  profiles: LLMProfile[];
  selected: number | null;
  onChange: (id: number | null) => void;
  disabled?: boolean;
  rightSlot?: React.ReactNode;
}) {
  const [open, setOpen] = React.useState(false);
  const activeDefault = profiles.find((p) => p.is_default);
  const current = selected != null ? profiles.find((p) => Number(p.id) === selected) : null;
  const label = current ? current.name : `默认${activeDefault ? `（${activeDefault.name}）` : ""}`;

  return (
    <div className="flex min-w-0 shrink-0 items-center gap-1 px-1 pt-0.5 pb-1">
      <ZapIcon className="size-3 shrink-0 text-muted-foreground/50" />
      <span className="truncate text-muted-foreground/70 text-xs" title={label}>
        {label}
      </span>
      <Popover open={open} onOpenChange={disabled ? undefined : setOpen}>
        <PopoverTrigger asChild>
          <button
            type="button"
            disabled={disabled}
            className="flex shrink-0 items-center gap-0.5 text-primary text-xs hover:underline disabled:pointer-events-none disabled:opacity-40"
          >
            更换
            <ChevronDownIcon className="size-3" />
          </button>
        </PopoverTrigger>
        <PopoverContent align="start" className="w-64 p-1">
          {/* default option */}
          <button
            type="button"
            onClick={() => {
              onChange(null);
              setOpen(false);
            }}
            className={cn(
              "flex w-full flex-col rounded px-2 py-1.5 text-left hover:bg-accent",
              selected == null && "bg-accent",
            )}
          >
            <span className="text-sm">默认{activeDefault ? `（${activeDefault.name}）` : ""}</span>
            {activeDefault && (
              <span className="text-[11px] text-muted-foreground">
                {activeDefault.format} · {activeDefault.model}
              </span>
            )}
          </button>
          {profiles.map((p) => (
            <button
              key={p.id}
              type="button"
              onClick={() => {
                onChange(Number(p.id));
                setOpen(false);
              }}
              className={cn(
                "flex w-full flex-col rounded px-2 py-1.5 text-left hover:bg-accent",
                selected === Number(p.id) && "bg-accent",
              )}
            >
              <span className="text-sm">{p.name}</span>
              <span className="text-[11px] text-muted-foreground">
                {p.format} · {p.model}
              </span>
            </button>
          ))}
        </PopoverContent>
      </Popover>
      {rightSlot}
    </div>
  );
}

// DraftChat is the default right-pane view: a fresh chat (agent picker in the
// header, centered empty state, composer) with NO conversation created yet. The
// conversation is created lazily on the first send (ChatGPT-style), then the
// parent switches to the real ChatView.
function DraftChat({
  agents,
  profiles,
  onStarted,
  presetAgentKey,
}: {
  agents: Agent[];
  profiles: LLMProfile[];
  onStarted: (c: Conversation, pending?: { input?: string; attachments?: ChatAttachment[] }) => void;
  // 「旧会话」创建入口带过来的预选 agent：存在即选中（允许切换）。
  presetAgentKey?: string;
}) {
  const [agentKey, setAgentKey] = React.useState("");
  const [llmProfileId, setLlmProfileId] = React.useState<number | null>(null);
  const [input, setInput] = React.useState("");
  const [sending, setSending] = React.useState(false);
  const [uploading, setUploading] = React.useState(false);

  // default the agent to Auto once agents load; the create-dialog preset wins.
  React.useEffect(() => {
    if (presetAgentKey && agents.some((a) => a.key === presetAgentKey)) {
      setAgentKey(presetAgentKey);
      return;
    }
    if (!agentKey && agents.some((a) => a.key === "auto")) setAgentKey("auto");
  }, [agentKey, agents, presetAgentKey]);

  const agent = agents.find((a) => a.key === agentKey);

  async function send() {
    const msg = input.trim();
    if (!msg || !agentKey || sending) return;
    setSending(true);
    try {
      const c = await api.createConversation(agentKey, "", llmProfileId);
      await api.sendConversationMessage(c.id, msg);
      onStarted(c);
    } catch (e) {
      toast.error(`发送失败：${(e as Error).message}`);
      setSending(false);
    }
  }

  // Attachments need a conversation to own the uploads dir (sessions/conv-<id>/),
  // and a draft has none yet — so picking a file CREATES the conversation, uploads
  // into it, then hands off to ChatView carrying the typed text + attachments (the
  // user sends from there). Mirrors the task main-agent console's upload, adapted to
  // the ChatGPT-style lazy-create flow.
  async function pickFiles(files: File[]) {
    if (files.length === 0 || !agentKey || uploading || sending) return;
    setUploading(true);
    try {
      const c = await api.createConversation(agentKey, "", llmProfileId);
      const r = await api.chatUpload("session", `conv-${c.id}`, files);
      onStarted(c, { input, attachments: r.attachments });
    } catch (e) {
      toast.error(`上传失败：${(e as Error).message}`);
      setUploading(false);
    }
  }

  const agentPicker = (
    <Select value={agentKey} onValueChange={setAgentKey}>
      <SelectTrigger className="w-full sm:w-40">
        <SelectValue placeholder="选择 Agent…" />
      </SelectTrigger>
      <SelectContent>
        <SelectGroup>
          {agents.map((a) => (
            <SelectItem key={a.key} value={a.key}>
              <span className="flex items-center gap-2">
                <Bot className="size-3.5" />
                {a.name}
                {!a.builtin && (
                  <Badge variant="outline" className="px-1 py-0 text-[9px]">
                    自定义
                  </Badge>
                )}
              </span>
            </SelectItem>
          ))}
        </SelectGroup>
      </SelectContent>
    </Select>
  );

  return (
    <>
      {/* empty / landing state fills the panel */}
      <div className="flex min-h-0 flex-1 flex-col items-center justify-center gap-2 px-4 text-center">
        <div className="flex size-12 items-center justify-center rounded-full bg-primary/10">
          <Bot className="size-6 text-primary" />
        </div>
        <div className="font-medium text-sm">开始和「{agent?.name ?? "Agent"}」对话</div>
        {agent?.description && <p className="max-w-md text-muted-foreground text-xs">{agent.description}</p>}
      </div>

      <Composer
        value={input}
        onChange={setInput}
        onSend={send}
        disabled={sending || uploading || !agentKey}
        placeholder="输入消息，@ 引用记录，Enter 发送"
        leftSlot={agentPicker}
        onPickFiles={pickFiles}
        uploading={uploading}
      />
      <LLMProfileRow
        profiles={profiles}
        selected={llmProfileId}
        onChange={setLlmProfileId}
        disabled={sending || uploading}
      />
    </>
  );
}

// ChatView is the right pane for one conversation — mirrors the task detail's
// main-agent console: header with agent + live badge + token/duration meta, a
// scroll-stick transcript (chat mode), and the composer.
function ChatView({
  conv,
  agents,
  profiles,
  initial,
  onTitleMaybeChanged,
  onConvUpdated,
}: {
  conv: Conversation;
  agents: Agent[];
  profiles: LLMProfile[];
  // pending text + already-uploaded attachments handed off from a draft that
  // created this conversation via the paperclip (consumed once, on mount).
  initial?: { input?: string; attachments?: ChatAttachment[] };
  onTitleMaybeChanged: () => void;
  onConvUpdated: () => void;
}) {
  const approvalFocus = useApprovalFocus({ conversationId: conv.id });
  const [messages, setMessages] = React.useState<Activity[]>([]);
  const [running, setRunning] = React.useState(false);
  const [input, setInput] = React.useState(initial?.input ?? "");
  const [sending, setSending] = React.useState(false);
  const [stopping, setStopping] = React.useState(false);
  // 方式1 文件上传:已上传的附件(落到 sessions/conv-<id>/uploads/),随下条消息一起发。
  const [attachments, setAttachments] = React.useState<ChatAttachment[]>(initial?.attachments ?? []);
  const [uploading, setUploading] = React.useState(false);
  const cursorRef = React.useRef(0); // newest loaded id — incremental-tail anchor
  const earliestRef = React.useRef(0); // earliest loaded id — reverse-pagination anchor
  const hasMoreRef = React.useRef(false); // older history remains above the loaded window
  const loadingMoreRef = React.useRef(false); // guard: one scroll-up load at a time
  const [historyLoaded, setHistoryLoaded] = React.useState(false);
  const [hasMore, setHasMore] = React.useState(false); // drives the "load earlier" hint
  const agent = agents.find((a) => a.key === conv.agent_key);
  const currentProfileId = conv.llm_profile_id ?? null;
  const side = useSideQuestions(`/api/conversations/${conv.id}`);

  async function changeProfile(id: number | null) {
    try {
      await api.updateConversationProfile(conv.id, id);
      onConvUpdated();
    } catch (e) {
      toast.error(`切换 LLM 失败：${(e as Error).message}`);
    }
  }

  // conversation-scoped detail fetcher for the reused Transcript renderer.
  const fetchDetail = React.useCallback((seq: number) => api.conversationMsgDetail(conv.id, seq), [conv.id]);

  // seq of the most-recent TodoWrite tool call (for the Todo popover); null if none.
  const latestTodoSeq = React.useMemo(() => {
    for (let i = messages.length - 1; i >= 0; i--) {
      const a = messages[i];
      if (a.kind === "tool_use" && a.tool === "TodoWrite") return a.seq;
    }
    return null;
  }, [messages]);

  // reset + load whenever the selected conversation changes. Load only the LATEST
  // page on open — a long thread's final answer sits at the very end, so the newest
  // page shows it immediately (issue #3: opening/refresh used to load the oldest
  // page, so the completed result was missing until another message advanced the
  // cursor). Older history streams in on scroll-up (loadEarlier below).
  React.useEffect(() => {
    cursorRef.current = 0;
    earliestRef.current = 0;
    hasMoreRef.current = false;
    setHasMore(false);
    setMessages([]);
    setHistoryLoaded(false);
    setRunning(false);
    let live = true;
    api
      .conversationHistory(conv.id, 0, HISTORY_PAGE)
      .then((r) => {
        if (!live) return;
        setMessages((current) => mergeActivities(r.items, current));
        cursorRef.current = Math.max(cursorRef.current, r.cursor);
        earliestRef.current = r.items.length ? r.items[0].seq : 0;
        hasMoreRef.current = r.hasMore;
        setHasMore(r.hasMore);
        setRunning((current) => current || r.running);
        setHistoryLoaded(true);
      })
      .catch(() => {
        // A source link can retry history through its locator; ordinary chats
        // retain the existing usable empty state on a transient failure.
        if (live) setHistoryLoaded(true);
      });
    return () => {
      live = false;
    };
  }, [conv.id]);

  // A trigger or another tab may start a turn while this conversation is open.
  // A false list snapshot must not stop the tail before its final messages load.
  React.useEffect(() => {
    if (conv.running) setRunning(true);
  }, [conv.running]);

  // poll while a turn is running: pull new steps after the cursor.
  React.useEffect(() => {
    if (!running) return;
    let live = true;
    let timer: ReturnType<typeof setTimeout>;
    const tick = async () => {
      let keepPolling = true;
      try {
        const r = await api.conversationMessages(conv.id, cursorRef.current);
        if (!live) return;
        if (r.items.length) {
          setMessages((prev) => mergeActivities(prev, r.items));
        }
        cursorRef.current = Math.max(cursorRef.current, r.cursor);
        keepPolling = r.running;
        setRunning(r.running);
        if (!r.running) onTitleMaybeChanged(); // first-turn auto-title landed
      } catch {
        /* transient — keep polling */
      } finally {
        // Slow responses must not overlap another poll with the same cursor.
        if (live && keepPolling) timer = setTimeout(() => void tick(), 1000);
      }
    };
    void tick();
    return () => {
      live = false;
      clearTimeout(timer);
    };
  }, [running, conv.id, onTitleMaybeChanged]);

  const loadFocusPage = React.useCallback(
    (before: number) => api.conversationHistory(conv.id, before, HISTORY_PAGE),
    [conv.id],
  );
  const mergeFocusPage = React.useCallback((page: { items: Activity[]; hasMore: boolean }) => {
    setMessages((prev) => mergeActivities(page.items, prev));
    earliestRef.current = page.items[0]?.seq ?? earliestRef.current;
    hasMoreRef.current = page.hasMore;
    setHasMore(page.hasMore);
  }, []);
  const focusHistory = useApprovalHistory(
    approvalFocus.state?.source,
    historyLoaded,
    messages,
    loadFocusPage,
    mergeFocusPage,
  );

  // ---- transcript auto-scroll (open → bottom; stick to bottom unless scrolled up) ----
  const contentRef = React.useRef<HTMLDivElement | null>(null);
  const atBottomRef = React.useRef(true);
  const viewport = React.useCallback(
    () => (contentRef.current?.closest('[data-slot="scroll-area-viewport"]') as HTMLElement | null) ?? null,
    [],
  );
  // Scroll-up loads one older page and prepends it, preserving the visual position
  // so the view doesn't jump (record height/offset before, restore the delta after).
  const loadEarlier = React.useCallback(async () => {
    if (loadingMoreRef.current || !hasMoreRef.current) return;
    const vp = viewport();
    if (!vp) return;
    loadingMoreRef.current = true;
    const prevH = vp.scrollHeight;
    const prevTop = vp.scrollTop;
    try {
      const r = await api.conversationHistory(conv.id, earliestRef.current, HISTORY_PAGE);
      if (r.items.length) {
        setMessages((prev) => mergeActivities(r.items, prev));
        earliestRef.current = r.items[0].seq;
      }
      hasMoreRef.current = r.hasMore;
      setHasMore(r.hasMore);
      requestAnimationFrame(() => {
        const v = viewport();
        if (v) v.scrollTop = prevTop + (v.scrollHeight - prevH);
      });
    } catch {
      /* transient — a later scroll retries */
    } finally {
      loadingMoreRef.current = false;
    }
  }, [conv.id, viewport]);
  React.useEffect(() => {
    const vp = viewport();
    if (!vp) return;
    const onScroll = () => {
      if (approvalFocus.state && !focusHistory.ready) return;
      atBottomRef.current = vp.scrollTop + vp.clientHeight >= vp.scrollHeight - 60;
      if (vp.scrollTop <= 80) void loadEarlier(); // near top → pull an older page
    };
    vp.addEventListener("scroll", onScroll, { passive: true });
    return () => vp.removeEventListener("scroll", onScroll);
  }, [viewport, loadEarlier, approvalFocus.state, focusHistory.ready]);
  // open/switch a conversation → jump to the latest (bottom)
  const lastConvIdRef = React.useRef(conv.id);
  const didInitialScrollRef = React.useRef(false);
  React.useLayoutEffect(() => {
    // 消息是异步加载的：挂载瞬间 scrollHeight=0，等首批消息到达再落底；
    // 换会话（非重挂载）时重置标记重新落底。
    if (lastConvIdRef.current !== conv.id) {
      lastConvIdRef.current = conv.id;
      didInitialScrollRef.current = false;
    }
    if (didInitialScrollRef.current || messages.length === 0) return;
    const vp = viewport();
    if (!vp) return;
    didInitialScrollRef.current = true;
    vp.scrollTop = vp.scrollHeight;
    atBottomRef.current = true;
  }, [messages.length, viewport, conv.id]);
  // new activity → stick to bottom only if the user is already pinned there
  React.useLayoutEffect(() => {
    if (approvalFocus.state || !atBottomRef.current) return;
    const vp = viewport();
    if (vp) vp.scrollTop = vp.scrollHeight;
  }, [viewport, approvalFocus.state, messages]);

  // Per-conversation token total, live — same accounting as the main-agent
  // console: completed runs' `result` sum + the in-progress run's latest `usage`.
  const tokenTotal = React.useMemo(() => {
    let i = 0,
      o = 0,
      cr = 0;
    let li = 0,
      lo = 0,
      lcr = 0;
    let turns = 0; // agent 循环轮次 = 模型调用次数（每次一条 kind='usage'）
    for (const a of messages) {
      if (a.kind === "result") {
        i += a.input_tokens ?? 0;
        o += a.output_tokens ?? 0;
        cr += a.cache_read_tokens ?? 0;
        li = lo = lcr = 0;
      } else if (a.kind === "usage") {
        turns += 1;
        li = a.input_tokens ?? 0;
        lo = a.output_tokens ?? 0;
        lcr = a.cache_read_tokens ?? 0;
      }
    }
    const I = i + li,
      O = o + lo,
      CR = cr + lcr;
    return { i: I, o: O, cr: CR, turns, any: I + O + CR > 0 };
  }, [messages]);

  // pickFiles uploads into this conversation's session dir (sessions/conv-<id>/
  // uploads/) and queues the returned metadata to send with the next message.
  async function pickFiles(files: File[]) {
    if (files.length === 0) return;
    setUploading(true);
    try {
      const r = await api.chatUpload("session", `conv-${conv.id}`, files);
      setAttachments((prev) => [...prev, ...r.attachments]);
    } catch (e) {
      toast.error(`上传失败：${(e as Error).message}`);
    } finally {
      setUploading(false);
    }
  }

  async function send() {
    const msg = input.trim();
    const atts = attachments;
    if (side.handleCommand(msg, () => setInput(""))) return;
    if ((!msg && atts.length === 0) || sending || running) return;
    setSending(true);
    setInput("");
    setAttachments([]);
    try {
      await api.sendConversationMessage(conv.id, msg, atts.length ? atts : undefined);
      // The live loop pulls the persisted human turn immediately. Sharing that
      // fetch avoids racing a separate post-send request against the poller.
      setRunning(true);
    } catch (e) {
      toast.error(`发送失败：${(e as Error).message}`);
      setInput(msg); // restore so the user doesn't lose their text
      setAttachments(atts); // and their attachments
    } finally {
      setSending(false);
    }
  }

  // stop aborts the in-flight run for this conversation. running flips to false on
  // the next 1s poll once the backend unwinds the agent; the trigger queue is not
  // affected — the agent's next queued fire still starts.
  async function stop() {
    if (stopping) return;
    setStopping(true);
    try {
      await api.stopConversation(conv.id);
    } catch (e) {
      toast.error(`停止失败：${(e as Error).message}`);
    } finally {
      setStopping(false);
    }
  }

  return (
    <SideQuestionWorkspace side={side} label={agent?.name ?? conv.agent_key} composerLayout="inline">
      {/* header: which agent + live + token meta */}
      <div className="flex min-w-0 flex-wrap items-center gap-2 border-b px-4 py-2.5">
        <Bot className="size-4 shrink-0 text-muted-foreground" />
        <span className="min-w-0 max-w-48 truncate font-medium text-sm">{agent?.name ?? conv.agent_key}</span>
        <span className="hidden shrink-0 font-mono text-muted-foreground text-xs sm:inline">{conv.agent_key}</span>
        {agent && !agent.builtin && (
          <Badge variant="outline" className="shrink-0 px-1.5 py-0 text-[10px]">
            自定义
          </Badge>
        )}
        {agent?.description && (
          <span className="min-w-0 truncate text-muted-foreground text-xs">{agent.description}</span>
        )}
        {running && <LiveBadge />}
        <SideQuestionButton side={side} />
        <div className="ml-auto flex min-w-0 max-w-full items-center justify-end gap-x-3 gap-y-1 text-muted-foreground text-xs max-sm:w-full max-sm:flex-wrap">
          {tokenTotal.turns > 0 && (
            <span title="agent 循环轮次（模型调用次数）" className="tabular-nums">
              {tokenTotal.turns} 轮
            </span>
          )}
          {tokenTotal.any && (
            <span title="input / cache(read) / output tokens" className="min-w-0 truncate tabular-nums">
              input {fmtTokens(tokenTotal.i)} · cache {fmtTokens(tokenTotal.cr)} · output {fmtTokens(tokenTotal.o)}
            </span>
          )}
        </div>
      </div>

      <ApprovalExecutionFocus focus={approvalFocus} history={focusHistory} />

      {/* messages */}
      <div className="relative flex min-h-0 min-w-0 flex-1">
      <ScrollArea type="auto" className="[&_[data-slot=scroll-area-viewport]>div]:block! min-h-0 min-w-0 flex-1">
        <div className="min-w-0 max-w-full px-4 py-3" ref={contentRef}>
          {messages.length === 0 && !running ? (
            <div className="py-10 text-center text-muted-foreground text-sm">
              开始和「{agent?.name ?? conv.agent_key}」对话
            </div>
          ) : (
            <>
              {hasMore && (
                <div className="pb-2 text-center text-[11px] text-muted-foreground/70">向上滚动加载更早的消息…</div>
              )}
              <Transcript
                activity={messages}
                live={running}
                chat
                fetchDetail={fetchDetail}
                focusedSeq={focusHistory.ready ? approvalFocus.state?.source?.seq : undefined}
              />
            </>
          )}
        </div>
      </ScrollArea>
      <JumpToBottom viewportOf={viewport} />
      </div>

      <Composer
        value={input}
        onChange={setInput}
        onSend={send}
        disabled={running || sending}
        allowBtw
        placeholder={running ? "Agent 正在回复，可输入 /btw 提问…" : "输入消息，@ 引用记录，Enter 发送"}
        running={running}
        onStop={stop}
        stopDisabled={stopping}
        attachments={attachments}
        onPickFiles={pickFiles}
        onRemoveAttachment={(path) => setAttachments((p) => p.filter((x) => x.path !== path))}
        uploading={uploading}
      />
      <LLMProfileRow
        profiles={profiles}
        selected={currentProfileId}
        onChange={changeProfile}
        disabled={running || sending}
        rightSlot={<TodoPopover seq={latestTodoSeq} fetchDetail={fetchDetail} />}
      />
    </SideQuestionWorkspace>
  );
}

// ConversationItem is one row in the legacy conversation list: title, agent
// subtitle, inline rename, pin marker, and a compact action menu.
const ConversationItem = React.memo(function ConversationItem({
  conv,
  agent,
  showAgent = true,
  active,
  renaming,
  renameText,
  onSelect,
  onStartRename,
  onRenameText,
  onCommitRename,
  onCancelRename,
  onTogglePinned,
  onDelete,
}: {
  conv: Conversation;
  agent?: Agent;
  showAgent?: boolean;
  active: boolean;
  renaming: boolean;
  renameText: string;
  onSelect: (id: number) => void;
  onStartRename: (conversation: Conversation) => void;
  onRenameText: (v: string) => void;
  onCommitRename: (id: number, title: string) => void;
  onCancelRename: () => void;
  onTogglePinned: (conversation: Conversation) => void;
  onDelete: (id: number) => void;
}) {
  const [deleteOpen, setDeleteOpen] = React.useState(false);
  const renameInputRef = React.useRef<HTMLInputElement>(null);
  const cancelRenameRef = React.useRef(false);
  const pinned = conversationIsPinned(conv);

  React.useEffect(() => {
    if (!renaming) return;
    cancelRenameRef.current = false;
    renameInputRef.current?.focus();
    renameInputRef.current?.select();
  }, [renaming]);

  return (
    <div
      className={cn(
        "group flex min-w-0 items-center gap-1 rounded-md pr-1 transition-colors",
        active ? "bg-accent text-accent-foreground" : "hover:bg-accent/50",
      )}
    >
      {renaming ? (
        <input
          ref={renameInputRef}
          value={renameText}
          onChange={(e) => onRenameText(e.target.value)}
          onBlur={() => {
            if (!cancelRenameRef.current) onCommitRename(conv.id, renameText);
          }}
          onKeyDown={(e) => {
            if (e.key === "Enter") onCommitRename(conv.id, renameText);
            if (e.key === "Escape") {
              e.preventDefault();
              cancelRenameRef.current = true;
              onCancelRename();
            }
          }}
          className="min-w-0 flex-1 rounded-md border border-input bg-background px-2 py-1 text-sm"
        />
      ) : (
        <button
          type="button"
          onClick={() => onSelect(conv.id)}
          onDoubleClick={() => onStartRename(conv)}
          title="双击重命名"
          className="min-w-0 flex-1 rounded-md px-2 py-1.5 text-left"
        >
          <div className="flex min-w-0 items-center gap-1.5">
            {pinned && <PinIcon className="size-3 shrink-0 text-primary" aria-label="已置顶" />}
            <div className="truncate text-sm">{conv.title || "新对话"}</div>
            {conv.running ? (
              <Badge variant="secondary" className="shrink-0 gap-1" title="Agent 正在运行">
                <Spinner className="size-3" aria-hidden="true" />
                运行中
              </Badge>
            ) : null}
          </div>
          <div className="flex min-w-0 items-center gap-1 text-[11px] text-muted-foreground">
            {showAgent && (
              <>
                <Bot className="size-3 shrink-0" />
                <span className="min-w-0 truncate">{agent?.name ?? conv.agent_key}</span>
                <span className="shrink-0">·</span>
              </>
            )}
            <span className="shrink-0">
              {new Date(conv.created_at).toLocaleDateString("zh-CN", {
                month: "numeric",
                day: "numeric",
                hour: "2-digit",
                minute: "2-digit",
              })}
            </span>
            <span className="shrink-0 opacity-60">#{conv.id}</span>
          </div>
        </button>
      )}
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            variant="ghost"
            size="icon-sm"
            className="shrink-0 text-muted-foreground"
            aria-label={`管理对话「${conv.title || "新对话"}」`}
          >
            <MoreHorizontalIcon />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuGroup>
            <DropdownMenuItem onSelect={() => onStartRename(conv)}>
              <PencilIcon />
              重命名
            </DropdownMenuItem>
            <DropdownMenuItem onSelect={() => onTogglePinned(conv)}>
              {pinned ? <PinOffIcon /> : <PinIcon />}
              {pinned ? "取消置顶" : "置顶"}
            </DropdownMenuItem>
          </DropdownMenuGroup>
          <DropdownMenuSeparator />
          <DropdownMenuGroup>
            <DropdownMenuItem variant="destructive" onSelect={() => setDeleteOpen(true)}>
              <Trash2Icon />
              删除
            </DropdownMenuItem>
          </DropdownMenuGroup>
        </DropdownMenuContent>
      </DropdownMenu>
      <AlertDialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除对话「{conv.title || "新对话"}」？</AlertDialogTitle>
            <AlertDialogDescription>此操作不可撤销。</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction onClick={() => onDelete(conv.id)}>删除</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
});

// AgentGroupHeader is the sticky, clickable divider above one agent's rows:
// collapse chevron, agent name, a running marker, and the row count.
function AgentGroupHeader({
  group,
  collapsed,
  hasActive,
  onToggle,
}: {
  group: AgentGroup;
  collapsed: boolean;
  hasActive: boolean;
  onToggle: (key: string) => void;
}) {
  return (
    <button
      type="button"
      onClick={() => onToggle(group.key)}
      aria-expanded={!collapsed}
      title={collapsed ? `展开「${group.name}」` : `收起「${group.name}」`}
      className={cn(
        "sticky top-0 z-10 flex min-w-0 items-center gap-1.5 rounded-md bg-card px-1.5 py-1 text-left font-medium text-[11px] transition-colors hover:bg-accent/50",
        collapsed && hasActive ? "text-foreground" : "text-muted-foreground",
      )}
    >
      <ChevronRightIcon className={cn("size-3 shrink-0 transition-transform", !collapsed && "rotate-90")} />
      <Bot className="size-3 shrink-0" />
      <span className="min-w-0 flex-1 truncate">{group.name}</span>
      {collapsed && hasActive && (
        <span className="size-1.5 shrink-0 rounded-full bg-primary" title="当前对话在此分组内" />
      )}
      {group.runningCount > 0 && (
        <Spinner className="size-3 shrink-0" aria-label={`${group.runningCount} 个对话运行中`} />
      )}
      <span className="shrink-0 tabular-nums opacity-60">{group.conversations.length}</span>
    </button>
  );
}

export default function ChatPage() {
  const [agents, setAgents] = React.useState<Agent[]>([]);
  const [profiles, setProfiles] = React.useState<LLMProfile[]>([]);
  const [sessions, setSessions] = React.useState<Session[]>([]);
  // 会话接口是否可用。后端还没上线 /api/sessions 时回退到旧对话列表。
  const [sessionsAvailable, setSessionsAvailable] = React.useState(true);
  const [convs, setConvs] = React.useState<Conversation[]>([]);
  const [query, setQuery] = React.useState<SessionListQuery>(DEFAULT_SESSION_LIST_QUERY);
  const [selectedSessionId, setSelectedSessionId] = React.useState<number | null>(null);
  const [selectedConvId, setSelectedConvId] = React.useState<number | null>(null);
  const [sourceRequested, setSourceRequested] = React.useState(false);
  const [renamingId, setRenamingId] = React.useState<number | null>(null);
  const [renameText, setRenameText] = React.useState("");
  const [renamingConvId, setRenamingConvId] = React.useState<number | null>(null);
  const [renameConvText, setRenameConvText] = React.useState("");
  const [visibleConversationCount, setVisibleConversationCount] = React.useState(CONVERSATION_LIST_PAGE);
  // Collapsed agent groups（只用于旧对话的「按 agent 分组」可选视图）。
  const [collapsedAgents, setCollapsedAgents] = React.useState<Set<string>>(() => new Set());
  // 会话列表收起的分区（键见 sessionSectionStateKey）。初值是首次默认，本地有记录时由
  // 下面的 effect 覆盖 —— 不在 useState 初始化里读 localStorage，免得水合不一致。
  const [collapsedSessionSections, setCollapsedSessionSections] = React.useState<Set<string>>(
    () => new Set(DEFAULT_COLLAPSED_SESSION_SECTIONS),
  );
  // Pending text + uploaded attachments handed off from a draft that created a
  // conversation via the paperclip, keyed by the new conversation id (consumed once
  // by ChatView on mount; ids never repeat, so leftover entries are harmless).
  const [pendingByConv, setPendingByConv] = React.useState<
    Record<number, { input?: string; attachments?: ChatAttachment[] }>
  >({});

  const sessionsSeq = React.useRef(0);
  const convsSeq = React.useRef(0);

  const selectSession = React.useCallback((id: number | null) => {
    setSelectedSessionId(id);
    if (id != null) setSelectedConvId(null);
  }, []);
  const selectConversation = React.useCallback((id: number | null) => {
    setSelectedConvId(id);
    if (id != null) setSelectedSessionId(null);
  }, []);

  const reloadSessions = React.useCallback(async () => {
    const seq = ++sessionsSeq.current;
    try {
      const items = await api.sessions();
      if (seq !== sessionsSeq.current) return;
      setSessions(items);
      setSessionsAvailable(true);
    } catch (error) {
      if (seq !== sessionsSeq.current) return;
      // 404/405 = 后端还没上线会话接口 ⇒ 明确回退到旧对话列表；其它错误按瞬时故障处理。
      if (error instanceof ApiError && (error.status === 404 || error.status === 405)) {
        setSessionsAvailable(false);
      }
    }
  }, []);

  const reloadConvs = React.useCallback(async () => {
    const seq = ++convsSeq.current;
    try {
      const items = await api.conversations();
      if (seq === convsSeq.current) setConvs(items);
    } catch {
      // Preserve the selected transcript and list on a transient poll failure.
    }
  }, []);

  React.useEffect(() => {
    api
      .agents()
      .then(setAgents)
      .catch(() => {
        /* Agent metadata is optional for rendering an existing conversation. */
      });
    api
      .llmProfiles()
      .then(setProfiles)
      .catch(() => {
        /* The conversation remains usable without profile labels. */
      });
  }, []);

  // 会话列表轮询：名册 presence / 状态点在动。
  React.useEffect(() => {
    let disposed = false;
    let timer: ReturnType<typeof setTimeout>;
    async function poll() {
      await reloadSessions();
      if (!disposed) timer = setTimeout(() => void poll(), SESSION_POLL_MS);
    }
    void poll();
    return () => {
      disposed = true;
      sessionsSeq.current++;
      clearTimeout(timer);
    };
  }, [reloadSessions]);

  // 旧对话列表轮询（迁移期仍然要能看到新对话与运行状态）。
  React.useEffect(() => {
    let disposed = false;
    let timer: ReturnType<typeof setTimeout>;
    async function poll() {
      await reloadConvs();
      if (!disposed) timer = setTimeout(() => void poll(), CONVERSATION_POLL_MS);
    }
    void poll();
    return () => {
      disposed = true;
      convsSeq.current++;
      clearTimeout(timer);
    };
  }, [reloadConvs]);

  React.useEffect(() => {
    const raw = getLocalStorageValue(COLLAPSED_AGENTS_KEY);
    if (!raw) return;
    try {
      const keys = JSON.parse(raw);
      if (Array.isArray(keys))
        setCollapsedAgents(new Set(keys.filter((key): key is string => typeof key === "string")));
    } catch {
      // Corrupted entry → start with everything expanded.
    }
  }, []);

  const toggleAgentCollapsed = React.useCallback((key: string) => {
    setCollapsedAgents((current) => {
      const next = new Set(current);
      if (!next.delete(key)) next.add(key);
      setLocalStorageValue(COLLAPSED_AGENTS_KEY, JSON.stringify([...next]));
      return next;
    });
  }, []);

  React.useEffect(() => {
    const raw = getLocalStorageValue(COLLAPSED_SESSION_SECTIONS_KEY);
    // 没有记录 ⇒ 保持首次默认（群聊 / 旧会话收起）。
    if (!raw) return;
    try {
      const keys = JSON.parse(raw);
      if (Array.isArray(keys))
        setCollapsedSessionSections(new Set(keys.filter((key): key is string => typeof key === "string")));
    } catch {
      // 记录损坏 ⇒ 保持首次默认。
    }
  }, []);

  // stateKey 是带视图前缀的完整键；整份集合写回本地（含仍是默认收起的分区）。
  const toggleSessionSection = React.useCallback((stateKey: string) => {
    setCollapsedSessionSections((current) => {
      const next = new Set(current);
      if (!next.delete(stateKey)) next.add(stateKey);
      setLocalStorageValue(COLLAPSED_SESSION_SECTIONS_KEY, JSON.stringify([...next]));
      return next;
    });
  }, []);

  // Restore the open thread from the URL on mount, so a refresh returns to the
  // same session/conversation instead of the empty draft view. Runs after
  // hydration (not a lazy useState init) to avoid a server/client mismatch.
  // 无 URL 参数时回退到 localStorage 的「上次会话」（A5：登录后回对话页能续上）。
  React.useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    setSourceRequested(params.has("approval"));
    const c = params.get("c");
    const s = params.get("s");
    const convId = c ? Number(c) : NaN;
    const sessionId = s ? Number(s) : NaN;
    if (Number.isFinite(sessionId)) setSelectedSessionId(sessionId);
    else if (Number.isFinite(convId)) setSelectedConvId(convId);
    else {
      try {
        const last = window.localStorage.getItem("artex_last_session");
        if (last?.startsWith("c:")) {
          const n = Number(last.slice(2));
          if (Number.isFinite(n) && n > 0) setSelectedConvId(n);
        } else if (last?.startsWith("s:")) {
          const n = Number(last.slice(2));
          if (Number.isFinite(n) && n > 0) setSelectedSessionId(n);
        }
      } catch {
        /* localStorage 不可用（隐私模式等）——停在草稿区 */
      }
    }
  }, []);
  // Mirror the current selection into the URL (replaceState → no history spam)
  // and persist it as「上次会话」。
  React.useEffect(() => {
    const url = new URL(window.location.href);
    if (selectedSessionId != null) {
      url.searchParams.set("s", String(selectedSessionId));
      url.searchParams.delete("c");
      try {
        window.localStorage.setItem("artex_last_session", `s:${selectedSessionId}`);
      } catch {
        /* ignore */
      }
    } else if (selectedConvId != null) {
      url.searchParams.set("c", String(selectedConvId));
      url.searchParams.delete("s");
      try {
        window.localStorage.setItem("artex_last_session", `c:${selectedConvId}`);
      } catch {
        /* ignore */
      }
    } else {
      url.searchParams.delete("s");
      url.searchParams.delete("c");
    }
    window.history.replaceState(null, "", url);
  }, [selectedSessionId, selectedConvId]);

  const agentByKey = React.useMemo(() => new Map(agents.map((agent) => [agent.key, agent])), [agents]);
  // conversation agents: custom agents + conversational built-ins (role=assistant,
  // e.g. Auto / 渗透测试). The orchestration built-ins (goals/planner/mainagent/worker)
  // are task-specific and stay hidden from the chat page.
  const chatAgents = React.useMemo(() => agents.filter((a) => !a.builtin || a.role === "assistant"), [agents]);

  // 旧对话与会话的对应关系（迁移兼容）：会话里带 conversation_id 时直接认；
  // 否则按「单一成员 + 标题与 agent 都对得上且候选唯一」回填出来的会话去认。
  const bindings = React.useMemo(() => bindLegacyConversations(sessions, convs), [sessions, convs]);
  const boundConvIds = React.useMemo(() => new Set(bindings.values()), [bindings]);
  // 没有会话的旧对话 —— 它们仍然是 agent 对话的唯一入口，必须留在列表里。
  const legacyConversations = React.useMemo(
    () => convs.filter((conversation) => !boundConvIds.has(conversation.id)),
    [convs, boundConvIds],
  );

  const visibleSessions = React.useMemo(
    () => filterSessions(sessions, query, agentByKey),
    [sessions, query, agentByKey],
  );
  const sessionSections = React.useMemo<SessionSection[]>(() => {
    if (query.view === "by_agent") return groupSessionsByAgent(visibleSessions, agentByKey);
    if (query.view === "by_attachment") return groupSessionsByAttachment(visibleSessions);
    // A3：默认按形态四分区（总控/群聊/大厅/旧会话）——不同种类的会话不再混排。
    return groupSessionsByKind(visibleSessions);
  }, [visibleSessions, query.view, agentByKey]);

  // 当前视图下收起的分区：剥掉视图前缀交给 SessionList，它只认 section.key。
  const collapsedSectionsInView = React.useMemo(() => {
    const prefix = sessionSectionStateKey(query.view, "");
    const keys = new Set<string>();
    for (const key of collapsedSessionSections) {
      if (key.startsWith(prefix)) keys.add(key.slice(prefix.length));
    }
    return keys;
  }, [collapsedSessionSections, query.view]);
  const toggleSectionInView = React.useCallback(
    (sectionKey: string) => toggleSessionSection(sessionSectionStateKey(query.view, sectionKey)),
    [toggleSessionSection, query.view],
  );
  const legacyCollapsed = collapsedSessionSections.has(LEGACY_CONVERSATIONS_SECTION);

  // 工具条的 agent 筛选选项：名册里出现过的 agent + 已知对话型 agent。
  const agentOptions = React.useMemo(() => {
    const counts = new Map<string, number>();
    for (const session of sessions) {
      for (const member of sessionMembers(session)) {
        counts.set(member.agent_key, (counts.get(member.agent_key) ?? 0) + 1);
      }
    }
    for (const conversation of convs) {
      counts.set(conversation.agent_key, (counts.get(conversation.agent_key) ?? 0) + 1);
    }
    const keys = new Set([...chatAgents.map((agent) => agent.key), ...counts.keys()]);
    if (query.agent) keys.add(query.agent);
    return [...keys]
      .map((key) => ({ key, name: agentByKey.get(key)?.name || key, count: counts.get(key) ?? 0 }))
      .sort((a, b) => a.name.localeCompare(b.name, "zh-CN"));
  }, [sessions, convs, chatAgents, agentByKey, query.agent]);

  const selectedSession = React.useMemo(
    () => sessions.find((session) => session.id === selectedSessionId) ?? null,
    [sessions, selectedSessionId],
  );
  const selectedConv = React.useMemo(
    () => convs.find((conversation) => conversation.id === selectedConvId) ?? null,
    [convs, selectedConvId],
  );
  // 选中会话对应的旧对话（迁移来的会话用它挂「旧对话记录」页签）。
  const boundConv = React.useMemo(() => {
    if (!selectedSession) return null;
    const convId = bindings.get(selectedSession.id);
    if (convId == null) return null;
    return convs.find((conversation) => conversation.id === convId) ?? null;
  }, [selectedSession, bindings, convs]);

  const del = React.useCallback(
    async (id: number) => {
      try {
        await api.deleteConversation(id);
        selectConversation(null);
        void reloadConvs();
      } catch (e) {
        toast.error(`删除失败：${(e as Error).message}`);
      }
    },
    [reloadConvs, selectConversation],
  );

  const togglePinned = React.useCallback(
    async (conversation: Conversation) => {
      const pinned = conversationIsPinned(conversation);
      try {
        await api.pinConversation(conversation.id, !pinned);
        void reloadConvs();
      } catch (e) {
        toast.error(`${pinned ? "取消置顶" : "置顶"}失败：${(e as Error).message}`);
      }
    },
    [reloadConvs],
  );

  const startRename = React.useCallback((c: Conversation) => {
    setRenamingConvId(c.id);
    setRenameConvText(c.title || "");
  }, []);
  const commitRename = React.useCallback(
    async (id: number, value: string) => {
      const title = value.trim();
      setRenamingConvId(null);
      if (!title) return;
      try {
        await api.renameConversation(id, title);
        void reloadConvs();
      } catch (e) {
        toast.error(`重命名失败：${(e as Error).message}`);
      }
    },
    [reloadConvs],
  );
  const cancelRename = React.useCallback(() => setRenamingConvId(null), []);

  // 会话的重命名 / 置顶 / 删除走 /api/sessions（§4.5）。
  const startRenameSession = React.useCallback((session: Session) => {
    setRenamingId(session.id);
    setRenameText(session.name);
  }, []);
  const commitRenameSession = React.useCallback(
    async (session: Session, value: string) => {
      const name = value.trim();
      setRenamingId(null);
      if (!name || name === session.name) return;
      try {
        await api.updateSession(session.id, { name });
        void reloadSessions();
      } catch (e) {
        toast.error(`重命名失败：${(e as Error).message}`);
      }
    },
    [reloadSessions],
  );
  const cancelRenameSession = React.useCallback(() => setRenamingId(null), []);

  const toggleSessionPinned = React.useCallback(
    async (session: Session) => {
      try {
        await api.updateSession(session.id, { pinned: !session.pinned_at });
        void reloadSessions();
      } catch (e) {
        toast.error(`${session.pinned_at ? "取消置顶" : "置顶"}失败：${(e as Error).message}`);
      }
    },
    [reloadSessions],
  );

  const deleteSession = React.useCallback(
    async (session: Session) => {
      try {
        await api.deleteSession(session.id);
        selectSession(null);
        void reloadSessions();
      } catch (e) {
        toast.error(`删除失败：${(e as Error).message}`);
      }
    },
    [reloadSessions, selectSession],
  );

  // 新建会话（三形态选择弹窗）：创建成功后插入列表并选中。
  const [createOpen, setCreateOpen] = React.useState(false);
  // 旧会话路径：从弹窗选好 agent 后滚回 DraftChat（首条消息懒创建，同迁移期旧对话流）。
  const [draftPresetAgent, setDraftPresetAgent] = React.useState("");
  const onSessionCreated = React.useCallback(
    (created: Session) => {
      setSessions((prev) => [created, ...prev.filter((item) => item.id !== created.id)]);
      selectSession(created.id);
    },
    [selectSession],
  );
  const onPickLegacyAgent = React.useCallback(
    (agentKey: string) => {
      setDraftPresetAgent(agentKey);
      selectSession(null);
      selectConversation(null);
    },
    [selectConversation, selectSession],
  );

  const legacyGroups = React.useMemo(
    () => groupByAgent(legacyConversations, agentByKey),
    [legacyConversations, agentByKey],
  );

  const legacyPanel = boundConv ? (
    <ChatView
      key={boundConv.id}
      conv={boundConv}
      agents={agents}
      profiles={profiles}
      onTitleMaybeChanged={reloadConvs}
      onConvUpdated={reloadConvs}
    />
  ) : undefined;

  let rightPane: React.ReactNode;
  if (selectedSession) {
    rightPane = (
      <SessionView
        key={selectedSession.id}
        session={selectedSession}
        agentByKey={agentByKey}
        candidates={chatAgents}
        legacyPanel={legacyPanel}
        onSessionChanged={() => void reloadSessions()}
        onSessionDeleted={(id) => {
          if (selectedSessionId === id) selectSession(null);
          void reloadSessions();
        }}
      />
    );
  } else if (selectedConv) {
    rightPane = (
      <ChatView
        key={selectedConv.id}
        conv={selectedConv}
        agents={agents}
        profiles={profiles}
        initial={pendingByConv[selectedConv.id]}
        onTitleMaybeChanged={reloadConvs}
        onConvUpdated={reloadConvs}
      />
    );
  } else if (sourceRequested) {
    rightPane = (
      <div role="status" className="p-6 text-muted-foreground text-sm">
        {selectedSessionId != null ? "会话已被删除" : "对话已被删除"}
      </div>
    );
  } else {
    rightPane = (
      <DraftChat
        agents={chatAgents}
        profiles={profiles}
        presetAgentKey={draftPresetAgent}
        onStarted={(c, pending) => {
          // Insert the new conversation immediately so `selected` resolves to
          // it on this render (switching to ChatView right away, before the
          // async reloadConvs lands); reloadConvs then reconciles titles etc.
          if (pending) setPendingByConv((p) => ({ ...p, [c.id]: pending }));
          setConvs((prev) => {
            if (prev.some((item) => item.id === c.id)) return prev;
            const firstUnpinned = prev.findIndex((item) => !conversationIsPinned(item));
            const insertAt = firstUnpinned < 0 ? prev.length : firstUnpinned;
            return [...prev.slice(0, insertAt), c, ...prev.slice(insertAt)];
          });
          selectConversation(c.id);
          void reloadConvs();
        }}
      />
    );
  }

  return (
    <div
      data-content-padding="false"
      className="flex h-[calc(100svh-3rem)] min-w-0 flex-col overflow-hidden p-3 sm:p-4 md:h-[calc(100svh-4rem)] md:p-6"
    >
      <div className="grid min-h-0 min-w-0 flex-1 grid-cols-1 grid-rows-[minmax(10rem,15rem)_minmax(0,1fr)] gap-3 md:grid-cols-[18rem_minmax(0,1fr)] md:grid-rows-[minmax(0,1fr)] md:gap-4">
        {/* left: 会话列表 */}
        <div className="flex flex-col overflow-hidden rounded-lg border bg-card">
          <div className="flex flex-col gap-2 border-b p-2">
            <Button size="sm" className="w-full" onClick={() => setCreateOpen(true)}>
              <PlusIcon />
              新建会话
            </Button>
            <SessionToolbar
              query={query}
              onQueryChange={setQuery}
              agentOptions={agentOptions}
              resultCount={visibleSessions.length}
              totalCount={sessions.length}
            />
          </div>
          <ScrollArea
            key={query.view}
            type="auto"
            className="[&_[data-slot=scroll-area-viewport]>div]:block! min-h-0 min-w-0 flex-1"
          >
            <div className="flex min-w-0 flex-col gap-0.5 p-2">
              {!sessionsAvailable && (
                <p className="rounded-md border border-dashed px-2 py-2 text-[11px] text-muted-foreground">
                  会话接口暂不可用，已回退到旧对话列表。
                </p>
              )}
              {sessionsAvailable && (
                <SessionList
                  sections={sessionSections}
                  selectedId={selectedSessionId}
                  onSelect={selectSession}
                  agentByKey={agentByKey}
                  collapsedSections={collapsedSectionsInView}
                  onToggleSection={toggleSectionInView}
                  renamingId={renamingId}
                  renameText={renameText}
                  onStartRename={startRenameSession}
                  onRenameText={setRenameText}
                  onCommitRename={(session, name) => void commitRenameSession(session, name)}
                  onCancelRename={cancelRenameSession}
                  onTogglePin={(session) => void toggleSessionPinned(session)}
                  onDelete={(session) => void deleteSession(session)}
                  emptyHint="暂无会话"
                />
              )}
              {legacyConversations.length > 0 && (
                <>
                  {/* 「旧对话」块同样是可折叠分区：固定键、各视图共用一份折叠态。 */}
                  <SessionSectionHeader
                    label="旧对话"
                    count={legacyConversations.length}
                    icon={<ListChecksIcon className="size-3 shrink-0" />}
                    collapsed={legacyCollapsed}
                    hasActive={legacyConversations.some((c) => c.id === selectedConvId)}
                    onToggle={() => toggleSessionSection(LEGACY_CONVERSATIONS_SECTION)}
                    className="mt-1"
                  />
                  {!legacyCollapsed &&
                    (query.view === "by_agent"
                      ? legacyGroups.map((group) => {
                          const collapsed = collapsedAgents.has(group.key);
                          return (
                            <React.Fragment key={group.key}>
                              <AgentGroupHeader
                                group={group}
                                collapsed={collapsed}
                                hasActive={group.conversations.some((c) => c.id === selectedConvId)}
                                onToggle={toggleAgentCollapsed}
                              />
                              {!collapsed &&
                                group.conversations.map((c) => (
                                  <ConversationItem
                                    key={c.id}
                                    conv={c}
                                    agent={agentByKey.get(c.agent_key)}
                                    showAgent={false}
                                    active={selectedConvId === c.id}
                                    renaming={renamingConvId === c.id}
                                    renameText={renamingConvId === c.id ? renameConvText : ""}
                                    onSelect={selectConversation}
                                    onStartRename={startRename}
                                    onRenameText={setRenameConvText}
                                    onCommitRename={(id, title) => void commitRename(id, title)}
                                    onCancelRename={cancelRename}
                                    onTogglePinned={(conversation) => void togglePinned(conversation)}
                                    onDelete={(id) => void del(id)}
                                  />
                                ))}
                            </React.Fragment>
                          );
                        })
                      : legacyConversations
                          .slice(0, visibleConversationCount)
                          .map((c) => (
                            <ConversationItem
                              key={c.id}
                              conv={c}
                              agent={agentByKey.get(c.agent_key)}
                              active={selectedConvId === c.id}
                              renaming={renamingConvId === c.id}
                              renameText={renamingConvId === c.id ? renameConvText : ""}
                              onSelect={selectConversation}
                              onStartRename={startRename}
                              onRenameText={setRenameConvText}
                              onCommitRename={(id, title) => void commitRename(id, title)}
                              onCancelRename={cancelRename}
                              onTogglePinned={(conversation) => void togglePinned(conversation)}
                              onDelete={(id) => void del(id)}
                            />
                          )))}
                  {!legacyCollapsed &&
                    query.view !== "by_agent" &&
                    visibleConversationCount < legacyConversations.length && (
                      <Button
                        type="button"
                        variant="ghost"
                        size="sm"
                        className="mt-1 w-full"
                        onClick={() => setVisibleConversationCount((count) => count + CONVERSATION_LIST_PAGE)}
                      >
                        加载更多
                      </Button>
                    )}
                </>
              )}
            </div>
          </ScrollArea>
        </div>

        {/* right: session view / legacy chat view */}
        <div className="flex min-w-0 flex-col overflow-hidden rounded-lg border bg-card">{rightPane}</div>
      </div>
      <CreateSessionDialog
        open={createOpen}
        onOpenChange={setCreateOpen}
        agents={chatAgents}
        onCreated={onSessionCreated}
        onPickLegacy={onPickLegacyAgent}
      />
    </div>
  );
}
