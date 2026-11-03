"use client";

import { Bot, ListFilterIcon, SearchIcon } from "lucide-react";

import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";

import type { SessionFilter, SessionListQuery, SessionSort, SessionViewMode } from "./session-format";

// ALL 是「不限」哨兵 —— Radix 的 SelectItem 不接受空字符串作为 value。
const ALL = "__all__";

export interface SessionAgentOption {
  key: string;
  name: string;
  count: number;
}

export interface SessionToolbarProps {
  query: SessionListQuery;
  onQueryChange: (query: SessionListQuery) => void;
  agentOptions: SessionAgentOption[];
  resultCount: number;
  totalCount: number;
}

// SessionToolbar 是左栏工具条：搜索 · 筛选 · 排序 · 视图。
// agent 是**筛选维度**（不是分组轴）—— 方案书 §2.1 共识 2 / §9 定稿 4。
export function SessionToolbar({ query, onQueryChange, agentOptions, resultCount, totalCount }: SessionToolbarProps) {
  const filterOptions: { value: SessionFilter; label: string }[] = [
    { value: "all", label: "全部" },
    { value: "unread", label: "有未读" },
    { value: "working", label: "工作中" },
    { value: "attention", label: "需关注" },
  ];
  const sortOptions: { value: SessionSort; label: string }[] = [
    { value: "updated", label: "最近" },
    { value: "created", label: "创建" },
  ];
  const viewOptions: { value: SessionViewMode; label: string }[] = [
    { value: "flat", label: "扁平" },
    { value: "by_attachment", label: "按工件分组" },
    { value: "by_agent", label: "按 agent 分组" },
  ];

  return (
    <div className="flex flex-col gap-2">
      <div className="relative">
        <SearchIcon className="pointer-events-none absolute top-1/2 left-2 size-3.5 -translate-y-1/2 text-muted-foreground" />
        <Input
          value={query.query}
          onChange={(event) => onQueryChange({ ...query, query: event.target.value })}
          placeholder="搜索会话"
          aria-label="搜索会话"
          className="h-8 pl-7 text-xs"
        />
      </div>
      <div className="flex min-w-0 items-center gap-1.5">
        <Select
          value={query.agent ?? ALL}
          onValueChange={(value) => onQueryChange({ ...query, agent: value === ALL ? null : value })}
        >
          <SelectTrigger size="sm" className="min-w-0 flex-1" aria-label="按 agent 筛选会话">
            <Bot />
            <SelectValue placeholder="全部 agent" />
          </SelectTrigger>
          <SelectContent>
            <SelectGroup>
              <SelectItem value={ALL}>全部 agent</SelectItem>
              {agentOptions.map((agent) => (
                <SelectItem key={agent.key} value={agent.key}>
                  {agent.name}（{agent.count}）
                </SelectItem>
              ))}
            </SelectGroup>
          </SelectContent>
        </Select>
        <Select
          value={query.filter}
          onValueChange={(value) => onQueryChange({ ...query, filter: value as SessionFilter })}
        >
          <SelectTrigger size="sm" className="min-w-0 flex-1" aria-label="筛选会话">
            <ListFilterIcon />
            <SelectValue placeholder="筛选" />
          </SelectTrigger>
          <SelectContent>
            <SelectGroup>
              {filterOptions.map((option) => (
                <SelectItem key={option.value} value={option.value}>
                  {option.label}
                </SelectItem>
              ))}
            </SelectGroup>
          </SelectContent>
        </Select>
        <Select value={query.sort} onValueChange={(value) => onQueryChange({ ...query, sort: value as SessionSort })}>
          <SelectTrigger size="sm" className="min-w-0 flex-1" aria-label="会话排序">
            <SelectValue placeholder="排序" />
          </SelectTrigger>
          <SelectContent>
            <SelectGroup>
              {sortOptions.map((option) => (
                <SelectItem key={option.value} value={option.value}>
                  {option.label}
                </SelectItem>
              ))}
            </SelectGroup>
          </SelectContent>
        </Select>
        <Select
          value={query.view}
          onValueChange={(value) => onQueryChange({ ...query, view: value as SessionViewMode })}
        >
          <SelectTrigger size="sm" className="min-w-0 flex-1" aria-label="会话列表视图">
            <SelectValue placeholder="视图" />
          </SelectTrigger>
          <SelectContent>
            <SelectGroup>
              {viewOptions.map((option) => (
                <SelectItem key={option.value} value={option.value}>
                  {option.label}
                </SelectItem>
              ))}
            </SelectGroup>
          </SelectContent>
        </Select>
      </div>
      <p className="px-1 text-muted-foreground text-xs tabular-nums">
        {resultCount === totalCount ? `共 ${totalCount} 个会话` : `${resultCount} / ${totalCount} 个会话`}
      </p>
    </div>
  );
}
