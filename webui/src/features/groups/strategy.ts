import type { GroupMember, GroupSelectStrategy } from "../../api/contracts";

/** 保留策略的展示文案，组内自动选择下拉与跨组批量选择对话框共用。 */
export const GROUP_SELECT_STRATEGY_OPTIONS: ReadonlyArray<{
  readonly value: GroupSelectStrategy;
  readonly label: string;
}> = [
  { value: "newest", label: "保留最新" },
  { value: "oldest", label: "保留最旧" },
  { value: "largest", label: "保留最大" },
  { value: "shortest_path", label: "保留最短路径" }
];

export function groupSelectStrategyText(strategy: GroupSelectStrategy): string {
  return GROUP_SELECT_STRATEGY_OPTIONS.find(option => option.value === strategy)?.label ?? strategy;
}

/** 返回负值表示 left 更应被保留；fileId 兜底保证结果确定。 */
function compareKeepPriority(left: GroupMember, right: GroupMember, strategy: GroupSelectStrategy): number {
  switch (strategy) {
    case "newest":
      return right.mtime - left.mtime || left.fileId - right.fileId;
    case "oldest":
      return left.mtime - right.mtime || left.fileId - right.fileId;
    case "largest":
      return right.size - left.size || left.fileId - right.fileId;
    case "shortest_path":
      return left.path.length - right.path.length
        || (left.path < right.path ? -1 : left.path > right.path ? 1 : 0)
        || left.fileId - right.fileId;
  }
}

/**
 * 组内自动选择（P0-3 第一步，纯前端）：在候选成员中按策略保留一者，返回其余应勾选删除的
 * fileId。调用方负责先排除 effective 代表与 Agent 离线成员——与后端 select-by-strategy
 * “策略保留者与 effective 代表永不入选”的语义对齐。
 */
export function pickStrategySelection(
  candidates: readonly GroupMember[],
  strategy: GroupSelectStrategy
): number[] {
  if (candidates.length === 0) return [];
  const keep = candidates.reduce((best, member) =>
    compareKeepPriority(member, best, strategy) < 0 ? member : best);
  return candidates
    .filter(member => member.fileId !== keep.fileId)
    .map(member => member.fileId);
}
