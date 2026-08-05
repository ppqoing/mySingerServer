import type { AgentStatus, GroupKind, GroupSort } from "../../api/contracts";

export interface GroupFiltersProps {
  readonly agents: readonly AgentStatus[];
  readonly kind: GroupKind;
  readonly machine: string;
  readonly minMembers: string;
  readonly query: string;
  readonly density: "compact" | "comfortable";
  readonly sort: GroupSort;
  readonly onDensityChange: (density: "compact" | "comfortable") => void;
  readonly onKindChange: (kind: GroupKind) => void;
  readonly onMachineChange: (machine: string) => void;
  readonly onMinMembersChange: (value: string) => void;
  readonly onQueryChange: (query: string) => void;
  readonly onSortChange: (sort: GroupSort) => void;
}

const kinds: Array<{ kind: GroupKind; label: string }> = [
  { kind: "exact", label: "精确重复" },
  { kind: "image", label: "相似图片" },
  { kind: "video", label: "相似视频" }
];

export function GroupFilters({
  agents,
  kind,
  machine,
  minMembers,
  query,
  density,
  sort,
  onDensityChange,
  onKindChange,
  onMachineChange,
  onMinMembersChange,
  onQueryChange,
  onSortChange
}: GroupFiltersProps) {
  const identifiedAgents = new Map<string, AgentStatus>();
  for (const agent of agents) {
    if (!agent.machineId) continue;
    const current = identifiedAgents.get(agent.machineId);
    const dispatchable = agent.online && agent.identityState === "claimed";
    const currentDispatchable = current?.online && current.identityState === "claimed";
    if (!current || (dispatchable && !currentDispatchable)) {
      identifiedAgents.set(agent.machineId, agent);
    }
  }

  return (
    <aside aria-label="重复组筛选" className="group-filters">
      <div aria-label="重复类型" className="group-filters__tabs" role="group">
        {kinds.map(item => (
          <button
            aria-pressed={kind === item.kind}
            className="group-filters__tab"
            key={item.kind}
            onClick={() => onKindChange(item.kind)}
            type="button"
          >
            {item.label}
          </button>
        ))}
      </div>

      <label className="group-filters__field">
        <span>路径搜索</span>
        <input aria-label="路径搜索" onChange={event => onQueryChange(event.target.value)} placeholder="按路径筛选" type="search" value={query} />
      </label>

      <label className="group-filters__field">
        <span>Agent</span>
        <select aria-label="Agent" onChange={event => onMachineChange(event.target.value)} value={machine}>
          <option value="">全部 Agent</option>
          {[...identifiedAgents.values()].map(agent => <option key={agent.machineId} value={agent.machineId}>{agent.machineId}</option>)}
        </select>
      </label>

      <label className="group-filters__field">
        <span>最少文件数</span>
        <input aria-label="最少文件数" min="1" onChange={event => onMinMembersChange(event.target.value)} placeholder="不限" type="number" value={minMembers} />
      </label>

      <label className="group-filters__field">
        <span>排序</span>
        <select aria-label="排序" onChange={event => onSortChange(event.target.value as GroupSort)} value={sort}>
          <option value="members_desc">文件数</option>
          <option value="newest">最新创建</option>
          <option value="reclaim_desc">可回收空间</option>
        </select>
      </label>

      <fieldset className="group-filters__density">
        <legend>密度</legend>
        <label><input checked={density === "compact"} name="density" onChange={() => onDensityChange("compact")} type="radio" />紧凑密度</label>
        <label><input checked={density === "comfortable"} name="density" onChange={() => onDensityChange("comfortable")} type="radio" />舒适密度</label>
      </fieldset>
    </aside>
  );
}
