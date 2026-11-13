import {
  Activity,
  Ban,
  BellRing,
  Bot,
  Brain,
  Bug,
  ClipboardList,
  Compass,
  FileText,
  FolderOpen,
  FolderSync,
  GitCompareArrows,
  History,
  LayoutDashboard,
  LibraryBig,
  type LucideIcon,
  MessageSquare,
  Network,
  Plug,
  Radio,
  ScrollText,
  Settings2,
  ShieldAlert,
  ShieldCheck,
  Sparkles,
  Target,
  Terminal,
  Users,
  Waypoints,
  Smartphone,
  Wrench,
} from "lucide-react";

export type NavBadge = "new" | "soon";

export interface NavSubItem {
  id: string;
  title: string;
  url: string;
  icon?: LucideIcon;
  badge?: NavBadge;
  disabled?: boolean;
  newTab?: boolean;
}

interface NavItemBase {
  id: string;
  title: string;
  icon?: LucideIcon;
  badge?: NavBadge;
  disabled?: boolean;
  newTab?: boolean;
}

export interface NavMainLinkItem extends NavItemBase {
  url: string;
  subItems?: never;
}

export interface NavMainParentItem extends NavItemBase {
  subItems: NavSubItem[];
}

export type NavMainItem = NavMainLinkItem | NavMainParentItem;

export interface NavGroup {
  id: number;
  label?: string;
  items: NavMainItem[];
}

export const sidebarItems: NavGroup[] = [
  {
    id: 1,
    label: "功能",
    items: [
      { id: "dashboard", title: "仪表盘", url: "/dashboard", icon: LayoutDashboard },
      { id: "chat", title: "对话", url: "/chat", icon: MessageSquare },
      { id: "tasks", title: "任务", url: "/function/tasks", icon: Target },
      { id: "task-hall", title: "任务大厅", url: "/task-hall", icon: Users },
      { id: "legacy", title: "传承图", url: "/legacy", icon: History },
      { id: "strategies", title: "策略库", url: "/strategies", icon: LibraryBig },
      { id: "sod", title: "职责分离", url: "/sod", icon: ShieldCheck },
      { id: "findings", title: "发现", url: "/function/findings", icon: Bug },
      { id: "traffic", title: "流量", url: "/function/traffic", icon: Activity },
      { id: "commands", title: "工具执行", url: "/function/commands", icon: Terminal },
      { id: "llm-records", title: "LLM 录制", url: "/function/llm-records", icon: Radio },
      { id: "assets", title: "资产", url: "/function/assets", icon: Network },
      { id: "business-graph", title: "业务建模", url: "/function/business-graph", icon: Waypoints },
      { id: "biz-diffs", title: "差异面板", url: "/function/business-graph/diffs", icon: GitCompareArrows },
      { id: "edge-exploration", title: "偏锋探索", url: "/function/business-graph", icon: Compass },
      { id: "miniprogram", title: "小程序渗透", url: "/function/miniprogram", icon: Smartphone },
      { id: "sync", title: "资产同步", url: "/function/sync", icon: FolderSync },
      { id: "workspace", title: "工作空间", url: "/function/workspace", icon: FolderOpen },
    ],
  },
  {
    id: 2,
    label: "系统",
    items: [
      { id: "llm", title: "LLM", url: "/system/llm", icon: Brain },
      { id: "agents", title: "Agent", url: "/system/agents", icon: Bot },
      { id: "mcp", title: "MCP", url: "/system/mcp", icon: Plug },
      { id: "skills", title: "Skill", url: "/system/skills", icon: Sparkles },
      { id: "tools", title: "工具", url: "/system/tools", icon: Wrench },
      { id: "notify", title: "通知推送", url: "/system/notify", icon: BellRing },
      { id: "intercept", title: "拦截规则", url: "/system/intercept", icon: ShieldAlert },
      { id: "asset-intercept", title: "资产拦截", url: "/system/intercept/assets", icon: Ban },
      { id: "approvals", title: "审批记录", url: "/system/intercept/approvals", icon: ClipboardList },
      { id: "logs", title: "日志", url: "/system/logs", icon: ScrollText },
      { id: "reports", title: "报告中心", url: "/system/reports", icon: FileText },
      { id: "settings", title: "系统配置", url: "/system/settings", icon: Settings2 },
    ],
  },
];
