"use client";

import * as React from "react";

import { ArrowDownIcon, PanelRightIcon } from "lucide-react";
import { toast } from "sonner";

import {
  MessageScroller,
  MessageScrollerButton,
  MessageScrollerContent,
  MessageScrollerProvider,
  MessageScrollerViewport,
  useMessageScroller,
} from "@/components/ui/message-scroller";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { api } from "@/lib/api";
import type { Session, SessionMember, SessionMessage } from "@/lib/session-types";
import type { Agent, ChatAttachment, TaskHallQuota } from "@/lib/types";
import { cn } from "@/lib/utils";

import { SessionComposer } from "./session-composer";
import { SessionHeader } from "./session-header";
import { SessionSidePanel } from "./session-side-panel";
import { SessionTimeline } from "./session-timeline";

const PRESENCE_POLL_MS = 5000;

function mergeMessages(prev: SessionMessage[], next: SessionMessage[]): SessionMessage[] {
  if (next.length === 0) return prev;
  const byId = new Map<number, SessionMessage>();
  for (const message of prev) byId.set(message.id, message);
  for (const message of next) byId.set(message.id, message);
  return [...byId.values()].sort((a, b) => a.id - b.id);
}

// ScrollToEndOnSend：自己发出消息后回到底部 —— 哪怕之前翻到了上面，发出去的话也得
// 看得见；回到底部的同时恢复贴底跟随，这一回合陆续到的回复会接着往下贴。
// useMessageScroller 只能在 MessageScrollerProvider 里调用，所以单拎成一个组件。
function ScrollToEndOnSend({ seq }: { seq: number }) {
  const { scrollToEnd } = useMessageScroller();
  React.useEffect(() => {
    if (seq > 0) scrollToEnd();
  }, [seq, scrollToEnd]);
  return null;
}

export interface SessionViewProps {
  session: Session;
  agentByKey: Map<string, Agent>;
  // 可加入的 agent 候选（对话型 agent）。
  candidates: Agent[];
  // 该会话由旧对话迁移而来时，旧对话记录面板（渲染在「旧对话记录」页签里）。
  legacyPanel?: React.ReactNode;
  onSessionChanged: () => void;
  onSessionDeleted: (id: number) => void;
}

// SessionView 是会话页主体：会话头（名字 + 花名册 + 状态）· 共享消息流 · 输入框。
export function SessionView({
  session,
  agentByKey,
  candidates,
  legacyPanel,
  onSessionChanged,
  onSessionDeleted,
}: SessionViewProps) {
  const [messages, setMessages] = React.useState<SessionMessage[]>([]);
  const [members, setMembers] = React.useState<SessionMember[]>(session.members ?? []);
  const [attachments, setAttachments] = React.useState(session.attachments ?? []);
  const [quota, setQuota] = React.useState<TaskHallQuota | null>(null);
  const [running, setRunning] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  const [panelOpen, setPanelOpen] = React.useState(false);
  // 页签始终默认「会话流」（群聊/总控的主界面）；「旧对话记录」只是迁移期的查阅
  // 入口，手点可达 —— 默认落进去会让群聊看起来「变回」旧会话。
  const [tab, setTab] = React.useState<"stream" | "legacy">("stream");
  const [uploading, setUploading] = React.useState(false);
  const [pendingAttachments, setPendingAttachments] = React.useState<ChatAttachment[]>([]);
  // 每成功发出一条消息 +1，驱动 ScrollToEndOnSend 回到底部（0 = 本次挂载还没发过）。
  const [sentSeq, setSentSeq] = React.useState(0);
  const cursorRef = React.useRef(0);

  // 换会话时重置：消息、名册、页签。
  // 只认 session.id —— 父组件每次轮询都会重建 session 对象（含新的 members 数组），
  // 跟着它重置会把刚拉到的消息和用户选的页签清掉。
  // biome-ignore lint/correctness/useExhaustiveDependencies: 只在换会话时重置。
  React.useEffect(() => {
    cursorRef.current = 0;
    setMessages([]);
    setMembers(session.members ?? []);
    setAttachments(session.attachments ?? []);
    setPendingAttachments([]);
    setTab("stream");
  }, [session.id]);

  // 首次拉取消息（since=0 取全量），之后按 cursor 增量。
  React.useEffect(() => {
    let live = true;
    api
      .sessionMessages(session.id, 0)
      .then((result) => {
        if (!live) return;
        setMessages(result.items);
        cursorRef.current = result.cursor;
        setRunning(result.running);
      })
      .catch(() => {
        /* 会话消息读不到时保留空态，不打断其它面板 */
      });
    return () => {
      live = false;
    };
  }, [session.id]);

  // 有回合在跑时轮询增量。
  React.useEffect(() => {
    if (!running) return;
    let live = true;
    let timer: ReturnType<typeof setTimeout>;
    const tick = async () => {
      let keepPolling = true;
      try {
        const result = await api.sessionMessages(session.id, cursorRef.current);
        if (!live) return;
        if (result.items.length) setMessages((prev) => mergeMessages(prev, result.items));
        cursorRef.current = Math.max(cursorRef.current, result.cursor);
        keepPolling = result.running;
        setRunning(result.running);
      } catch {
        /* 瞬时失败：下一轮重试 */
      } finally {
        if (live && keepPolling) timer = setTimeout(() => void tick(), 1000);
      }
    };
    void tick();
    return () => {
      live = false;
      clearTimeout(timer);
    };
  }, [running, session.id]);

  // 名册 / 关联 / 额度：低频刷新（详情 + presence）。
  React.useEffect(() => {
    let live = true;
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      try {
        const detail = await api.session(session.id);
        if (!live) return;
        if (detail.members) setMembers(detail.members);
        if (detail.attachments) setAttachments(detail.attachments);
      } catch {
        /* 详情接口不可用时用列表里带的摘要 */
      }
      try {
        const presence = await api.sessionPresence(session.id);
        if (live && presence.length) setMembers(presence);
      } catch {
        /* presence 读不到时保留上一次的名册 */
      }
      if (live) timer = setTimeout(() => void poll(), PRESENCE_POLL_MS);
    };
    void poll();
    return () => {
      live = false;
      clearTimeout(timer);
    };
  }, [session.id]);

  // 任务大厅型会话顺带读额度（没配额度时后端回 404 ⇒ 客户端兜成 null）。
  React.useEffect(() => {
    const groupId = session.group_id;
    if (!groupId) {
      setQuota(null);
      return;
    }
    let live = true;
    api
      .taskHallQuota(groupId)
      .then((value) => {
        if (live) setQuota(value);
      })
      .catch(() => {
        if (live) setQuota(null);
      });
    return () => {
      live = false;
    };
  }, [session.group_id]);

  async function rename(name: string) {
    try {
      await api.updateSession(session.id, { name });
      onSessionChanged();
    } catch (error) {
      toast.error(`重命名失败：${(error as Error).message}`);
    }
  }

  async function togglePin() {
    try {
      await api.updateSession(session.id, { pinned: !session.pinned_at });
      onSessionChanged();
    } catch (error) {
      toast.error(`置顶失败：${(error as Error).message}`);
    }
  }

  async function removeSession() {
    try {
      await api.deleteSession(session.id);
      onSessionDeleted(session.id);
    } catch (error) {
      toast.error(`删除失败：${(error as Error).message}`);
    }
  }

  async function addMember(agentKey: string) {
    setBusy(true);
    try {
      await api.addSessionMember(session.id, agentKey);
      onSessionChanged();
    } catch (error) {
      toast.error(`加入失败：${(error as Error).message}`);
    } finally {
      setBusy(false);
    }
  }

  // 更新成员在场状态（PATCH /sessions/{id}/members/{agent}）：working/blocked/
  // listening/idle。唤醒器会把 working 覆盖回 listening，所以这里主要是把卡住的
  // 成员标 blocked、或把误标的拨回 listening。
  async function setPresence(agentKey: string, presence: string) {
    setBusy(true);
    try {
      await api.updateSessionMemberPresence(session.id, agentKey, { presence });
      onSessionChanged();
    } catch (error) {
      toast.error(`状态更新失败：${(error as Error).message}`);
    } finally {
      setBusy(false);
    }
  }

  async function removeMember(agentKey: string) {
    setBusy(true);
    try {
      await api.removeSessionMember(session.id, agentKey);
      onSessionChanged();
    } catch (error) {
      toast.error(`移出失败：${(error as Error).message}`);
    } finally {
      setBusy(false);
    }
  }

  async function pickFiles(files: File[]) {
    if (files.length === 0) return;
    setUploading(true);
    try {
      const result = await api.chatUpload("session", `session-${session.id}`, files);
      setPendingAttachments((prev) => [...prev, ...result.attachments]);
    } catch (error) {
      toast.error(`上传失败：${(error as Error).message}`);
    } finally {
      setUploading(false);
    }
  }

  async function send(body: string, toAgent: string) {
    const seated = members.some((member) => member.agent_key === toAgent);
    if (toAgent && !seated) {
      // @ 到不在席的 agent ⇒ 先热加入再定向（§4.3 的加入方式②）。
      try {
        await api.addSessionMember(session.id, toAgent);
      } catch (error) {
        toast.error(`加入会话失败：${(error as Error).message}`);
        return;
      }
    }
    try {
      await api.postSessionMessage(session.id, {
        body,
        to_agent: toAgent,
        kind: "note",
        attachments: pendingAttachments.length ? pendingAttachments : undefined,
      });
      setPendingAttachments([]);
      setRunning(true);
      setTab("stream");
      setSentSeq((seq) => seq + 1);
    } catch (error) {
      toast.error(`发送失败：${(error as Error).message}`);
    }
  }

  const sessionForPanel: Session = { ...session, members, attachments };
  const tabs = legacyPanel ? (
    <div className="flex items-center gap-0.5 rounded-md border p-0.5">
      <button
        type="button"
        onClick={() => setTab("stream")}
        className={cn(
          "rounded px-2 py-1 text-xs transition-colors",
          tab === "stream" ? "bg-accent text-accent-foreground" : "text-muted-foreground hover:text-foreground",
        )}
      >
        会话流
      </button>
      <button
        type="button"
        onClick={() => setTab("legacy")}
        className={cn(
          "rounded px-2 py-1 text-xs transition-colors",
          tab === "legacy" ? "bg-accent text-accent-foreground" : "text-muted-foreground hover:text-foreground",
        )}
      >
        旧对话记录
      </button>
    </div>
  ) : undefined;

  return (
    <div className="flex min-h-0 min-w-0 flex-1 flex-col">
      <SessionHeader
        session={sessionForPanel}
        members={members}
        agentByKey={agentByKey}
        candidates={candidates}
        onRename={(name) => void rename(name)}
        onTogglePin={() => void togglePin()}
        onDelete={() => void removeSession()}
        onAddMember={(agentKey) => void addMember(agentKey)}
        onRemoveMember={(agentKey) => void removeMember(agentKey)}
        onSetPresence={(agentKey, presence) => void setPresence(agentKey, presence)}
        tabs={tabs}
        aside={
          <button
            type="button"
            onClick={() => setPanelOpen(true)}
            className="rounded p-1 text-muted-foreground hover:text-foreground"
            title="关联与状态"
            aria-label="打开关联与状态面板"
          >
            <PanelRightIcon className="size-4" />
          </button>
        }
        busy={busy}
      />

      {tab === "legacy" && legacyPanel ? (
        <div className="flex min-h-0 min-w-0 flex-1 flex-col">{legacyPanel}</div>
      ) : (
        <>
          {/* 消息流：打开即落在最新消息（defaultScrollPosition="end"，换会话 = 重新挂载 = 重新落底）；
              停在底部时新消息自动跟随（autoScroll —— 用户往上翻了就不再拽回去）；离开顶 / 底端时
              浮出「回到顶部 / 回到底部」，贴端自动隐藏（显隐由组件按可滚方向切换）。 */}
          <MessageScrollerProvider autoScroll defaultScrollPosition="end">
            <MessageScroller className="min-w-0 flex-1">
              <MessageScrollerViewport aria-label="会话消息">
                <MessageScrollerContent className="gap-3 px-4 py-3">
                  <SessionTimeline
                    messages={messages}
                    agentByKey={agentByKey}
                    emptyHint="这个会话还没有消息 —— 在下面输入，或 @ 一个 agent 让它加入。"
                  />
                </MessageScrollerContent>
              </MessageScrollerViewport>
              {/* 两个按钮都给 ArrowDown：start 方向由 MessageScrollerButton 的样式自动转 180°。
                  常驻可见（用户要求一键直达，不要「滚离才浮现」的智能显隐）：覆盖库默认的
                  data-[active=false] 隐藏态（透明+平移出界+禁点）。 */}
              <MessageScrollerButton
                direction="start"
                title="回到顶部"
                className="data-[active=false]:pointer-events-auto data-[active=false]:translate-y-0 data-[active=false]:scale-100 data-[active=false]:opacity-100"
              >
                <ArrowDownIcon />
                <span className="sr-only">回到顶部</span>
              </MessageScrollerButton>
              <MessageScrollerButton
                direction="end"
                title="回到底部（最新消息）"
                className="data-[active=false]:pointer-events-auto data-[active=false]:translate-y-0 data-[active=false]:scale-100 data-[active=false]:opacity-100"
              >
                <ArrowDownIcon />
                <span className="sr-only">回到底部</span>
              </MessageScrollerButton>
              <ScrollToEndOnSend seq={sentSeq} />
            </MessageScroller>
          </MessageScrollerProvider>
          <SessionComposer
            members={members}
            candidates={candidates}
            agentByKey={agentByKey}
            onSend={send}
            onPickFiles={pickFiles}
            attachments={pendingAttachments}
            onRemoveAttachment={(path) => setPendingAttachments((prev) => prev.filter((item) => item.path !== path))}
            uploading={uploading}
            running={running}
          />
        </>
      )}

      <Sheet open={panelOpen} onOpenChange={setPanelOpen}>
        <SheetContent side="right">
          <SheetHeader>
            <SheetTitle>关联与状态</SheetTitle>
            <SheetDescription>会话关联的工件、参与 agent 的状态与配额。</SheetDescription>
          </SheetHeader>
          <div className="min-h-0 flex-1 overflow-y-auto">
            <SessionSidePanel session={sessionForPanel} members={members} agentByKey={agentByKey} quota={quota} />
          </div>
        </SheetContent>
      </Sheet>
    </div>
  );
}
