export interface NavigationItem {
  readonly label: string;
  readonly to: string;
}

export const navigation: readonly NavigationItem[] = [
  { label: "总览", to: "/overview" },
  { label: "Agent", to: "/agents" },
  { label: "扫描任务", to: "/scans" },
  { label: "一筛分析", to: "/analysis" },
  { label: "重复组", to: "/groups" },
  { label: "删除审计", to: "/audit" },
  { label: "GUI 设置", to: "/settings" }
];
