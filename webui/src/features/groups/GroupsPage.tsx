import { useEffect, useMemo, useRef, useState } from "react";
import { appApi } from "../../api/appApi";
import { isAbortError } from "../../api/client";
import type {
  AgentStatus,
  AppApi,
  DeleteTaskStatus,
  GroupDetail as GroupDetailModel,
  GroupKind,
  GroupMember,
  GroupQuery,
  GroupSelectStrategy,
  GroupSort,
  GroupsStats,
  GroupsStatsQuery
} from "../../api/contracts";
import { apiErrorText } from "../../api/errorText";
import { Modal } from "../../components/Modal";
import { usePagedGroups } from "../../hooks/usePagedGroups";
import { usePolling } from "../../hooks/usePolling";
import { useSelection } from "../../hooks/useSelection";
import { DeleteDialog } from "../deletion/DeleteDialog";
import {
  deriveDeleteRetryPlan,
  type DeleteReviewMember,
  type DeleteReviewSnapshot
} from "./deleteReview";
import { byteText } from "./format";
import { GroupDetail } from "./GroupDetail";
import { GroupFilters } from "./GroupFilters";
import { GroupTable } from "./GroupTable";
import { GROUP_SELECT_STRATEGY_OPTIONS, groupSelectStrategyText, pickStrategySelection } from "./strategy";
import "./GroupsPage.css";

export interface GroupsPageProps {
  readonly api?: AppApi;
  readonly activeDeleteTaskId?: string;
  readonly deleteExecutionPending?: boolean;
  readonly deleteReviewSnapshot?: DeleteReviewSnapshot;
  readonly onActiveDeleteTaskIdChange?: (taskId: string | undefined) => void;
  readonly onDeleteExecutionPendingChange?: (pending: boolean) => void;
  readonly onDeleteReviewSnapshotChange?: (snapshot: DeleteReviewSnapshot | undefined) => void;
  readonly onRequestDelete?: (memberIds: number[]) => void;
  /** 审计页一键重试（P1-6）交办的重试 fileIds；消费后经 onRetryFileIdsConsumed 清空。 */
  readonly retryFileIds?: readonly number[];
  readonly onRetryFileIdsConsumed?: () => void;
}

interface DetailRequestState {
  readonly key: string | undefined;
  readonly data: GroupDetailModel | undefined;
  readonly error: Error | undefined;
}

interface SelectionOwnerLedger {
  readonly scopeKey: string;
  readonly owners: ReadonlyMap<number, DeleteReviewMember>;
}

/** 等 scope 切换在 useSelection 内生效后再落的选择负载（跨组批量/行快捷/重试恢复共用）。 */
interface PendingSelection {
  readonly scopeKey: string;
  readonly ids: readonly number[];
  readonly owners: ReadonlyMap<number, DeleteReviewMember>;
  readonly notice: string;
}

const EMPTY_AGENTS: readonly AgentStatus[] = [];
const EMPTY_OWNERS: ReadonlyMap<number, DeleteReviewMember> = new Map();

function mediaQuery(query: string): boolean {
  return typeof window !== "undefined" && typeof window.matchMedia === "function" && window.matchMedia(query).matches;
}

function useNarrowScreen(query: string): boolean {
  const [matches, setMatches] = useState(() => mediaQuery(query));
  useEffect(() => {
    if (typeof window === "undefined" || typeof window.matchMedia !== "function") return undefined;
    const current = window.matchMedia(query);
    const update = (event: MediaQueryListEvent) => setMatches(event.matches);
    current.addEventListener("change", update);
    return () => current.removeEventListener("change", update);
  }, [query]);
  return matches;
}

function currentQuery(kind: GroupKind, page: number, q: string, machine: string, minMembers: string, sort: GroupSort): GroupQuery {
  const parsedMinimum = Number(minMembers);
  return {
    kind,
    page,
    size: 100,
    ...(q ? { q } : {}),
    ...(machine ? { machine } : {}),
    ...(Number.isSafeInteger(parsedMinimum) && parsedMinimum > 0 ? { minMembers: parsedMinimum } : {}),
    sort
  };
}

function unavailableMachines(onlineMachineIds: ReadonlySet<string>, detail: GroupDetailModel | undefined): ReadonlySet<string> {
  return new Set((detail?.members ?? [])
    .filter(member => !onlineMachineIds.has(member.machineId))
    .map(member => member.machineId));
}

export function GroupsPage({
  activeDeleteTaskId: controlledActiveDeleteTaskId,
  api = appApi,
  deleteExecutionPending: controlledDeleteExecutionPending,
  deleteReviewSnapshot: controlledDeleteReviewSnapshot,
  onActiveDeleteTaskIdChange,
  onDeleteExecutionPendingChange,
  onDeleteReviewSnapshotChange,
  onRequestDelete,
  onRetryFileIdsConsumed,
  retryFileIds
}: GroupsPageProps) {
  const [kind, setKind] = useState<GroupKind>("exact");
  const [page, setPage] = useState(1);
  const [searchInput, setSearchInput] = useState("");
  const [debouncedSearch, setDebouncedSearch] = useState("");
  const [machine, setMachine] = useState("");
  const [minMembersInput, setMinMembersInput] = useState("");
  const [debouncedMinMembers, setDebouncedMinMembers] = useState("");
  const [sort, setSort] = useState<GroupSort>("members_desc");
  const [density, setDensity] = useState<"compact" | "comfortable">("compact");
  const [selectedGroupId, setSelectedGroupId] = useState<number | undefined>(undefined);
  const [memberPage, setMemberPage] = useState(1);
  const [detailReload, setDetailReload] = useState(0);
  const [detailSession, setDetailSession] = useState(0);
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [deleteLocked, setDeleteLocked] = useState(false);
  const [localActiveDeleteTaskId, setLocalActiveDeleteTaskId] = useState<string>();
  const [localDeleteExecutionPending, setLocalDeleteExecutionPending] = useState(false);
  const [localDeleteReviewSnapshot, setLocalDeleteReviewSnapshot] = useState<DeleteReviewSnapshot>();
  const [selectionNotice, setSelectionNotice] = useState<string>();
  const [terminalDeleteTaskId, setTerminalDeleteTaskId] = useState<string>();
  const terminalDeleteTasksRef = useRef(new Set<string>());
  const [selectionOwners, setSelectionOwners] = useState<SelectionOwnerLedger>({
    scopeKey: "exact:none",
    owners: EMPTY_OWNERS
  });
  const [detailRequest, setDetailRequest] = useState<DetailRequestState>({
    key: undefined,
    data: undefined,
    error: undefined
  });
  // scopeOverride 服务于不打开详情的选择：跨组批量选择为 `kind:multi`，行内“选中其余”为 `kind:groupId`。
  const [scopeOverride, setScopeOverride] = useState<string>();
  const [pendingSelection, setPendingSelection] = useState<PendingSelection>();
  const [representativePending, setRepresentativePending] = useState(false);
  const [representativeNotice, setRepresentativeNotice] = useState<string>();
  const [representativeError, setRepresentativeError] = useState<string>();
  const [batchSelectOpen, setBatchSelectOpen] = useState(false);
  const [batchStrategy, setBatchStrategy] = useState<GroupSelectStrategy>("newest");
  const [batchPending, setBatchPending] = useState(false);
  const [batchError, setBatchError] = useState<string>();
  const [autoOpenDelete, setAutoOpenDelete] = useState(false);
  const isDrawer = useNarrowScreen("(max-width: 1279px)");
  const isMobile = useNarrowScreen("(max-width: 719px)");
  const activeDeleteTaskId = onActiveDeleteTaskIdChange === undefined
    ? localActiveDeleteTaskId
    : controlledActiveDeleteTaskId;
  const deleteExecutionPending = onDeleteExecutionPendingChange === undefined
    ? localDeleteExecutionPending
    : controlledDeleteExecutionPending ?? false;
  const deleteReviewSnapshot = onDeleteReviewSnapshotChange === undefined
    ? localDeleteReviewSnapshot
    : controlledDeleteReviewSnapshot;
  const agentState = usePolling(signal => api.listAgents(signal), { dependencies: [api] });
  const agents = agentState.data ?? EMPTY_AGENTS;
  const agentVerificationReady = agentState.data !== undefined && agentState.error === undefined;
  const verifiedOnlineMachineIds = useMemo(
    () => new Set(agentState.error
      ? []
      : agents
        .filter(agent => agent.online && agent.identityState === "claimed" && agent.machineId)
        .map(agent => agent.machineId)),
    [agentState.error, agents]
  );
  const verifiedOnlineMachineKey = useMemo(
    () => [...verifiedOnlineMachineIds].sort().join("\u0000"),
    [verifiedOnlineMachineIds]
  );

  useEffect(() => {
    const effectiveSearch = searchInput.trim();
    if (effectiveSearch === debouncedSearch) return undefined;
    const timer = window.setTimeout(() => {
      setDebouncedSearch(effectiveSearch);
      setPage(1);
    }, 300);
    return () => window.clearTimeout(timer);
  }, [debouncedSearch, searchInput]);

  // 最少文件数与路径搜索同一防抖模式：击键不立即发查询，300ms 后生效并重置页码。
  useEffect(() => {
    const effectiveMinMembers = minMembersInput.trim();
    if (effectiveMinMembers === debouncedMinMembers) return undefined;
    const timer = window.setTimeout(() => {
      setDebouncedMinMembers(effectiveMinMembers);
      setPage(1);
    }, 300);
    return () => window.clearTimeout(timer);
  }, [debouncedMinMembers, minMembersInput]);

  const query = useMemo(
    () => currentQuery(kind, page, debouncedSearch, machine, debouncedMinMembers, sort),
    [debouncedSearch, debouncedMinMembers, kind, machine, page, sort]
  );
  const groups = usePagedGroups(api, query);
  // 当前筛选的聚合统计：跟随防抖后的筛选值（不含页码/排序）；加载或失败时静默不显示该行。
  const filterStatsQuery = useMemo<GroupsStatsQuery>(() => {
    const parsedMinimum = Number(debouncedMinMembers);
    return {
      kind,
      ...(debouncedSearch ? { q: debouncedSearch } : {}),
      ...(machine ? { machine } : {}),
      ...(Number.isSafeInteger(parsedMinimum) && parsedMinimum > 0 ? { minMembers: parsedMinimum } : {})
    };
  }, [debouncedMinMembers, debouncedSearch, kind, machine]);
  const [filterStats, setFilterStats] = useState<{ query: GroupsStatsQuery; stats: GroupsStats }>();
  const [statsVersion, setStatsVersion] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    const requested = filterStatsQuery;
    void api.getGroupsStats(requested, controller.signal).then(
      stats => {
        if (!controller.signal.aborted) setFilterStats({ query: requested, stats });
      },
      () => { /* 统计行静默降级：失败时不显示 */ }
    );
    return () => controller.abort();
  }, [api, filterStatsQuery, statsVersion]);
  const detailKey = selectedGroupId === undefined
    ? undefined
    : `${kind}:${selectedGroupId}:${memberPage}:${detailReload}:${detailSession}`;

  useEffect(() => {
    if (selectedGroupId === undefined || detailKey === undefined) return undefined;
    const controller = new AbortController();
    void api.getGroup(selectedGroupId, memberPage, 100, controller.signal).then(
      value => {
        if (!controller.signal.aborted) {
          setDetailRequest({ key: detailKey, data: value, error: undefined });
        }
      },
      error => {
        if (!controller.signal.aborted && !isAbortError(error)) {
          setDetailRequest({
            key: detailKey,
            data: undefined,
            error: error instanceof Error ? error : new Error("读取重复组详情失败")
          });
        }
      }
    );
    return () => controller.abort();
  }, [api, detailKey, memberPage, selectedGroupId]);

  const detailIsCurrent = detailKey !== undefined && detailRequest.key === detailKey;
  const detail = detailIsCurrent ? detailRequest.data : undefined;
  const detailError = detailIsCurrent ? detailRequest.error : undefined;
  const detailLoading = detailKey !== undefined && !detailIsCurrent;
  const selectionScope = scopeOverride ??
    (selectedGroupId === undefined ? `${kind}:none` : `${kind}:${selectedGroupId}`);
  const activeSelectionOwners = selectionOwners.scopeKey === selectionScope ? selectionOwners.owners : EMPTY_OWNERS;
  const unavailableMachineIds = useMemo(
    () => unavailableMachines(verifiedOnlineMachineIds, detail),
    [detail, verifiedOnlineMachineIds]
  );
  const protectedIds = useMemo(() => {
    const protectedSet = new Set<number>();
    if (detail?.representativeFileId !== null && detail?.representativeFileId !== undefined) {
      protectedSet.add(detail.representativeFileId);
    }
    for (const candidate of detail?.members ?? []) {
      if (unavailableMachineIds.has(candidate.machineId)) protectedSet.add(candidate.fileId);
    }
    for (const [fileId, owner] of activeSelectionOwners) {
      if (!verifiedOnlineMachineIds.has(owner.machineId)) protectedSet.add(fileId);
    }
    return protectedSet;
  }, [activeSelectionOwners, detail, unavailableMachineIds, verifiedOnlineMachineIds]);
  const selection = useSelection(selectionScope, protectedIds);

  // 选择摘要：设备数取 owner 台账 machineId 去重，大小从台账/当前详情成员补齐。
  const selectionTotals = useMemo(() => {
    let totalSize = 0;
    const machines = new Set<string>();
    for (const fileId of selection.selectedIds) {
      const owner = activeSelectionOwners.get(fileId);
      const member = detail?.members.find(candidate => candidate.fileId === fileId);
      const machineId = owner?.machineId ?? member?.machineId;
      if (machineId) machines.add(machineId);
      const size = owner?.size ?? member?.size;
      if (typeof size === "number" && Number.isFinite(size) && size > 0) totalSize += size;
    }
    return { totalSize, machineCount: machines.size };
  }, [activeSelectionOwners, detail, selection.selectedIds]);

  // 删除确认页展示的保留文件：当前详情已加载成员页中未被勾选的成员（含代表文件）。
  // 跨组批量或未加载分页无法穷举全部未选中成员——口径上至少保证代表在列（代表在当前页时）。
  const keptMembers = useMemo<readonly DeleteReviewMember[]>(() =>
    (detail?.members ?? [])
      .filter(member => !selection.selectedSet.has(member.fileId))
      .map(member => ({ fileId: member.fileId, machineId: member.machineId, path: member.path })),
  [detail, selection.selectedSet]);

  // useSelection 在渲染期过滤掉已离线的已选项；这里依据 owner 台账补一条用户可见通知，
  // 并从台账移除这些条目。Agent 轮询失败（状态不可核验）走既有的 role="alert" 通道，不重复提示。
  useEffect(() => {
    if (agentState.error !== undefined) return undefined;
    const removed = [...activeSelectionOwners.values()].filter(
      owner => !verifiedOnlineMachineIds.has(owner.machineId)
    );
    if (removed.length === 0) return undefined;
    const timer = window.setTimeout(() => {
      setSelectionNotice(`${removed.length} 项因 Agent 离线被移除。`);
      setSelectionOwners(current => {
        if (current.scopeKey !== selectionScope) return current;
        const owners = new Map(current.owners);
        for (const owner of removed) owners.delete(owner.fileId);
        return { scopeKey: selectionScope, owners };
      });
    }, 0);
    return () => window.clearTimeout(timer);
  // 台账清理会回流到本 effect 的依赖，移除后 removed 为空自然终止。
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeSelectionOwners, agentState.error, selectionScope, verifiedOnlineMachineKey]);

  function resetSelectionOwners(scopeKey: string) {
    setSelectionOwners({ scopeKey, owners: new Map() });
  }

  function toggleMember(fileId: number) {
    const candidate = detail?.members.find(member => member.fileId === fileId);
    if (!candidate) return;
    const isSelected = selection.selectedSet.has(fileId);
    if (!isSelected && protectedIds.has(fileId)) return;
    setSelectionNotice(undefined);
    setSelectionOwners(current => {
      const owners = new Map(current.scopeKey === selectionScope ? current.owners : EMPTY_OWNERS);
      if (isSelected) {
        owners.delete(fileId);
      } else {
        owners.set(fileId, {
          fileId,
          machineId: candidate.machineId,
          path: candidate.path,
          size: candidate.size
        });
      }
      return { scopeKey: selectionScope, owners };
    });
    selection.toggle(fileId);
  }

  function changeKind(next: GroupKind) {
    if (next === kind) return;
    setKind(next);
    resetFilteredReview(`${next}:none`);
  }

  function resetFilteredReview(nextScope = `${kind}:none`, resetPage = true) {
    if (selection.selectedIds.length > 0) {
      setSelectionNotice("筛选条件已变化，已清除原有选择。");
    }
    if (resetPage) setPage(1);
    setSelectedGroupId(undefined);
    setMemberPage(1);
    setScopeOverride(undefined);
    setPendingSelection(undefined);
    resetSelectionOwners(nextScope);
  }

  function changeFilter<T extends string>(update: (value: T) => void, value: T) {
    update(value);
    resetFilteredReview();
  }

  function changeSearch(value: string) {
    setSearchInput(value);
    resetFilteredReview(`${kind}:none`, false);
  }

  function changeMinMembers(value: string) {
    setMinMembersInput(value);
    resetFilteredReview(`${kind}:none`, false);
  }

  function openGroup(id: number) {
    const nextScope = `${kind}:${id}`;
    setScopeOverride(undefined);
    setPendingSelection(undefined);
    setRepresentativeNotice(undefined);
    setRepresentativeError(undefined);
    if (nextScope !== selectionScope) {
      if (selection.selectedIds.length > 0) {
        setSelectionNotice("已切换分组，已清空原选择。");
      } else {
        setSelectionNotice(undefined);
      }
      resetSelectionOwners(nextScope);
    } else {
      setSelectionNotice(undefined);
    }
    setSelectedGroupId(id);
    setMemberPage(1);
    setDetailSession(value => value + 1);
  }

  function selectAllCurrentPage(select: boolean) {
    if (!detail) return;
    for (const candidate of detail.members) {
      if (protectedIds.has(candidate.fileId)) continue;
      if (selection.selectedSet.has(candidate.fileId) !== select) toggleMember(candidate.fileId);
    }
  }

  // P0-2：指定保留副本。成功后刷新组详情；若该成员已在删除选择中则自动移除并提示。
  async function setRepresentative(member: GroupMember) {
    if (!detail || representativePending) return;
    if (!window.confirm(`将文件 #${member.fileId} 设为该组的保留副本？\n${member.machineId}：${member.path}`)) return;
    setRepresentativePending(true);
    setRepresentativeNotice(undefined);
    setRepresentativeError(undefined);
    try {
      await api.setGroupRepresentative(detail.id, member.fileId);
      const wasSelected = selection.selectedSet.has(member.fileId);
      if (wasSelected) toggleMember(member.fileId);
      setRepresentativeNotice(wasSelected
        ? `已将文件 #${member.fileId} 设为保留副本，并从删除选择中移除。`
        : `已将文件 #${member.fileId} 设为保留副本。`);
      setDetailReload(value => value + 1);
    } catch (error) {
      if (!isAbortError(error)) setRepresentativeError(apiErrorText(error, "设置保留副本失败"));
    } finally {
      setRepresentativePending(false);
    }
  }

  // P0-3 第一步（纯前端）：基于已加载成员页按策略保留一者、勾选其余。
  function applyAutoSelect(strategy: GroupSelectStrategy) {
    if (!detail) return;
    const candidates = detail.members.filter(member =>
      member.fileId !== detail.representativeFileId && !unavailableMachineIds.has(member.machineId));
    const ids = pickStrategySelection(candidates, strategy);
    for (const fileId of ids) {
      if (!selection.selectedSet.has(fileId)) toggleMember(fileId);
    }
    const partial = detail.memberTotal > detail.memberSize
      ? "成员超过 100 个，仅覆盖当前已加载成员。"
      : "";
    setSelectionNotice(`已按「${groupSelectStrategyText(strategy)}」选中 ${ids.length} 项。${partial}`);
  }

  // P0-3 组行快捷操作：不打开详情，保留代表、选中其余可删除成员（仅覆盖首页 100 个成员）。
  async function selectOthersInGroup(groupId: number) {
    try {
      const groupDetail = await api.getGroup(groupId, 1, 100);
      const candidates = groupDetail.members.filter(member =>
        member.fileId !== groupDetail.representativeFileId && verifiedOnlineMachineIds.has(member.machineId));
      if (candidates.length === 0) {
        setSelectionNotice(`重复组 #${groupId} 没有可选中的其余成员（代表与离线成员已排除）。`);
        return;
      }
      const scopeKey = `${kind}:${groupId}`;
      const owners = new Map<number, DeleteReviewMember>(candidates.map(member => [member.fileId, {
        fileId: member.fileId,
        machineId: member.machineId,
        path: member.path,
        size: member.size
      }]));
      const partial = groupDetail.memberTotal > groupDetail.members.length
        ? "成员超过 100 个，仅覆盖已加载成员。"
        : "";
      setSelectedGroupId(undefined);
      setMemberPage(1);
      setScopeOverride(scopeKey);
      setPendingSelection({
        scopeKey,
        ids: candidates.map(member => member.fileId),
        owners,
        notice: `已选中重复组 #${groupId} 的其余 ${candidates.length} 个成员（保留代表文件）。${partial}`
      });
    } catch (error) {
      if (!isAbortError(error)) setSelectionNotice(apiErrorText(error, "选中其余成员失败"));
    }
  }

  // P0-3 第二步：跨组批量选择。API 仅返回 fileIds，无法补齐 machineId/path，owner 台账留空——
  // 删除终态核对时 deriveDeleteRetryPlan 映射不到问题项，将保守回退为整批重选（deleteReview.ts）。
  async function applyBatchSelection() {
    if (batchPending) return;
    setBatchPending(true);
    setBatchError(undefined);
    try {
      const parsedMinimum = Number(debouncedMinMembers);
      const result = await api.selectGroupsByStrategy({
        kind,
        ...(debouncedSearch ? { q: debouncedSearch } : {}),
        ...(machine ? { machine } : {}),
        ...(Number.isSafeInteger(parsedMinimum) && parsedMinimum > 0 ? { minMembers: parsedMinimum } : {}),
        strategy: batchStrategy
      });
      setBatchSelectOpen(false);
      const scopeKey = `${kind}:multi`;
      const base = `已按「${groupSelectStrategyText(batchStrategy)}」选中 ${result.fileIds.length} 项（覆盖 ${result.groups} 个重复组）。`;
      const notice = result.truncated
        ? `${base}已达上限，仅选中前 ${result.fileIds.length} 个。`
        : base;
      setSelectedGroupId(undefined);
      setMemberPage(1);
      setScopeOverride(scopeKey);
      setPendingSelection({ scopeKey, ids: result.fileIds, owners: new Map(), notice });
    } catch (error) {
      if (!isAbortError(error)) setBatchError(apiErrorText(error, "批量选择失败"));
    } finally {
      setBatchPending(false);
    }
  }

  function closeDetail() {
    setSelectedGroupId(undefined);
    setMemberPage(1);
    setScopeOverride(undefined);
    setPendingSelection(undefined);
    setRepresentativeNotice(undefined);
    setRepresentativeError(undefined);
    resetSelectionOwners(`${kind}:none`);
  }

  function requestDelete() {
    if (deleteLocked || deleteExecutionPending) return;
    if (activeDeleteTaskId) {
      setDeleteOpen(true);
      return;
    }
    if (selection.selectedIds.length === 0) return;
    const snapshot = [...selection.selectedIds];
    if (onRequestDelete) {
      onRequestDelete(snapshot);
      return;
    }
    setDeleteOpen(true);
  }

  function setActiveDeleteTaskId(taskId: string | undefined) {
    if (onActiveDeleteTaskIdChange) {
      onActiveDeleteTaskIdChange(taskId);
      return;
    }
    setLocalActiveDeleteTaskId(taskId);
  }

  function setDeleteExecutionPending(pending: boolean) {
    if (onDeleteExecutionPendingChange) {
      onDeleteExecutionPendingChange(pending);
      return;
    }
    setLocalDeleteExecutionPending(pending);
  }

  function setDeleteReviewSnapshot(snapshot: DeleteReviewSnapshot | undefined) {
    if (onDeleteReviewSnapshotChange) {
      onDeleteReviewSnapshotChange(snapshot);
      return;
    }
    setLocalDeleteReviewSnapshot(snapshot);
  }

  function startDeleteExecution(memberIds: number[]) {
    const members = memberIds.map(fileId => {
      const candidate = activeSelectionOwners.get(fileId) ??
        detail?.members.find(member => member.fileId === fileId);
      return {
        fileId,
        machineId: candidate?.machineId ?? "",
        path: candidate?.path ?? "",
        size: candidate?.size
      };
    });
    if (selectedGroupId === undefined) return;
    setDeleteReviewSnapshot({
      groupId: selectedGroupId,
      kind,
      scopeKey: selectionScope,
      members
    });
  }

  function closeDelete() {
    if (deleteLocked) return;
    setDeleteOpen(false);
    if (terminalDeleteTaskId === undefined) return;
    if (activeDeleteTaskId === terminalDeleteTaskId) setActiveDeleteTaskId(undefined);
    setTerminalDeleteTaskId(undefined);
    const snapshot = deleteReviewSnapshot;
    if (!snapshot?.terminalStatus || snapshot.reconciled) {
      setDeleteReviewSnapshot(undefined);
      return;
    }
    if (selectedGroupId === undefined && selection.selectedIds.length === 0) {
      const nextScope = `${snapshot.kind}:${snapshot.groupId}`;
      setKind(snapshot.kind);
      setSelectedGroupId(snapshot.groupId);
      setMemberPage(1);
      setDetailSession(value => value + 1);
      resetSelectionOwners(nextScope);
      setSelectionNotice("正在返回原重复组并恢复失败或不确定项。");
    }
  }

  function acceptDelete(taskId: string) {
    setActiveDeleteTaskId(taskId);
    setTerminalDeleteTaskId(undefined);
  }

  function refreshGroupsAndStats() {
    groups.invalidateAll();
    setStatsVersion(value => value + 1);
  }

  function reconcileDeleteResult(
    status: DeleteTaskStatus,
    snapshot: DeleteReviewSnapshot
  ): boolean {
    // 删除已改变组数据：丢弃全部缓存页并重取当前页，避免翻页读到删除前的陈旧数据。
    refreshGroupsAndStats();
    const snapshotIds = new Set(snapshot.members.map(member => member.fileId));
    const unrelatedIds = selection.selectedIds.filter(fileId => !snapshotIds.has(fileId));
    const { hasIssues, retryMembers } = deriveDeleteRetryPlan(status, snapshot);
    if (retryMembers.some(member => !verifiedOnlineMachineIds.has(member.machineId))) {
      setSelectionNotice("失败项所属 Agent 离线；已保留结果，等待 Agent 恢复后再启用重试。");
      return false;
    }

    const nextIds = [...new Set([
      ...unrelatedIds,
      ...retryMembers.map(member => member.fileId)
    ])];
    selection.replace(nextIds);
    const owners = new Map<number, DeleteReviewMember>();
    for (const fileId of unrelatedIds) {
      const owner = activeSelectionOwners.get(fileId);
      if (owner) owners.set(fileId, owner);
    }
    for (const member of retryMembers) owners.set(member.fileId, member);
    setSelectionOwners({ scopeKey: selectionScope, owners });
    setSelectionNotice(hasIssues
      ? "失败或不确定项已保留，可关闭结果后重新检查并重试。"
      : undefined);
    setDetailReload(value => value + 1);
    return true;
  }

  function finishDelete(status: DeleteTaskStatus) {
    if (terminalDeleteTasksRef.current.has(status.taskId)) return;
    terminalDeleteTasksRef.current.add(status.taskId);
    setActiveDeleteTaskId(status.taskId);
    setTerminalDeleteTaskId(status.taskId);
    const snapshot = deleteReviewSnapshot;
    if (!snapshot) {
      refreshGroupsAndStats();
      if (scopeOverride === `${kind}:multi`) {
        // 跨组批量选择没有逐项核对快照：终态后直接清空选择并退出 multi scope，
        // 避免对可能已删除的文件再次发起删除。
        setScopeOverride(undefined);
        setPendingSelection(undefined);
        resetSelectionOwners(`${kind}:none`);
        setSelectionNotice("删除任务已完成，已清空跨组选择；逐组结果请前往删除审计页核对。");
      } else {
        setSelectionNotice("删除任务已完成；当前视图没有可核对的原始选择快照。");
      }
      return;
    }
    if (snapshot.scopeKey !== selectionScope) {
      refreshGroupsAndStats();
      setDeleteReviewSnapshot({ ...snapshot, terminalStatus: status, reconciled: false });
      setSelectionNotice("另一重复组的删除任务已完成；当前选择保持不变。");
      return;
    }
    if (!agentVerificationReady) {
      refreshGroupsAndStats();
      setDeleteReviewSnapshot({ ...snapshot, terminalStatus: status, reconciled: false });
      setSelectionNotice("删除结果已保留；等待 Agent 状态核验后恢复可重试项。");
      return;
    }
    const reconciled = reconcileDeleteResult(status, snapshot);
    setDeleteReviewSnapshot({ ...snapshot, terminalStatus: status, reconciled });
  }

  useEffect(() => {
    const snapshot = deleteReviewSnapshot;
    if (!snapshot?.terminalStatus || snapshot.reconciled || pendingSelection !== undefined ||
      selectedGroupId !== undefined || selection.selectedIds.length > 0) return;
    const timer = window.setTimeout(() => {
      const nextScope = `${snapshot.kind}:${snapshot.groupId}`;
      setKind(snapshot.kind);
      setSelectedGroupId(snapshot.groupId);
      setMemberPage(1);
      setDetailSession(value => value + 1);
      resetSelectionOwners(nextScope);
      setSelectionNotice("正在返回原重复组并恢复失败或不确定项。");
    }, 0);
    return () => window.clearTimeout(timer);
  }, [deleteReviewSnapshot, pendingSelection, selectedGroupId, selection.selectedIds.length]);

  useEffect(() => {
    const snapshot = deleteReviewSnapshot;
    if (!snapshot?.terminalStatus || !agentVerificationReady || pendingSelection !== undefined ||
      snapshot.scopeKey !== selectionScope) return;
    const timer = window.setTimeout(() => {
      const { retryMembers } = deriveDeleteRetryPlan(snapshot.terminalStatus!, snapshot);
      const hasOfflineRetry = retryMembers.some(
        member => !verifiedOnlineMachineIds.has(member.machineId)
      );
      if (snapshot.reconciled) {
        if (hasOfflineRetry) {
          setDeleteReviewSnapshot({ ...snapshot, reconciled: false });
          setSelectionNotice("失败项所属 Agent 离线；已保留结果，等待 Agent 恢复后再启用重试。");
        }
        return;
      }
      if (reconcileDeleteResult(snapshot.terminalStatus!, snapshot)) {
        setDeleteReviewSnapshot({ ...snapshot, reconciled: true });
      }
    }, 0);
    return () => window.clearTimeout(timer);
  // Reconciliation is intentionally keyed to the persisted snapshot and current review scope.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [agentVerificationReady, deleteReviewSnapshot, selectionScope, verifiedOnlineMachineKey]);

  // 待落选择（跨组批量/行内“选中其余”/一键重试恢复）：等 useSelection 的 scope 切换生效后再写入，
  // 避免 replace 被 render 期的 scope 重置吞掉。
  useEffect(() => {
    if (!pendingSelection || pendingSelection.scopeKey !== selectionScope) return undefined;
    const pending = pendingSelection;
    const timer = window.setTimeout(() => {
      selection.replace(pending.ids);
      setSelectionOwners({ scopeKey: pending.scopeKey, owners: new Map(pending.owners) });
      setSelectionNotice(pending.notice);
      setPendingSelection(undefined);
    }, 0);
    return () => window.clearTimeout(timer);
  // 与既有恢复/核对 effect 同一模式：只跟随负载与 scope。
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pendingSelection, selectionScope]);

  // P1-6 审计页一键重试接入：等首轮 Agent 核验就绪后按快照恢复选择并自动打开删除准备。
  useEffect(() => {
    if (!retryFileIds || retryFileIds.length === 0) return undefined;
    if (!agentVerificationReady) {
      // 首轮核验进行中：等待；核验失败（状态不可信）则放弃恢复。
      if (agentState.error === undefined) return undefined;
      const timer = window.setTimeout(() => {
        onRetryFileIdsConsumed?.();
        setSelectionNotice("Agent 状态不可用，无法恢复待重试选择。");
      }, 0);
      return () => window.clearTimeout(timer);
    }
    const timer = window.setTimeout(() => {
      onRetryFileIdsConsumed?.();
      const snapshot = deleteReviewSnapshot;
      const wanted = new Set(retryFileIds);
      const members = snapshot?.members.filter(member => wanted.has(member.fileId)) ?? [];
      if (!snapshot || members.length === 0) {
        setSelectionNotice("缺少可核对的原始选择，无法一键重试。");
        return;
      }
      const online = members.filter(member => verifiedOnlineMachineIds.has(member.machineId));
      if (online.length === 0) {
        setSelectionNotice("待重试项所属 Agent 离线，无法恢复选择。");
        return;
      }
      // 快照随本次交办消费掉；执行重试时 startDeleteExecution 会重建新快照。
      setKind(snapshot.kind);
      setScopeOverride(undefined);
      setSelectedGroupId(snapshot.groupId);
      setMemberPage(1);
      setDetailSession(value => value + 1);
      setDetailReload(value => value + 1);
      setActiveDeleteTaskId(undefined);
      setTerminalDeleteTaskId(undefined);
      setDeleteReviewSnapshot(undefined);
      setPendingSelection({
        scopeKey: snapshot.scopeKey,
        ids: online.map(member => member.fileId),
        owners: new Map(online.map(member => [member.fileId, member])),
        notice: `已恢复 ${online.length} 个待重试项，确认后将重新发起删除。`
      });
      setAutoOpenDelete(true);
    }, 0);
    return () => window.clearTimeout(timer);
  // 恢复只应跟随交办负载与 Agent 核验状态；快照读取以消费时为准。
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [retryFileIds, agentVerificationReady, agentState.error, verifiedOnlineMachineKey]);

  // 待落选择写完后自动打开删除准备（一键重试的第二步）。
  useEffect(() => {
    if (!autoOpenDelete || pendingSelection !== undefined) return undefined;
    const timer = window.setTimeout(() => {
      setAutoOpenDelete(false);
      if (selection.selectedIds.length === 0) {
        setSelectionNotice("待重试项均不可选（Agent 离线或已受保护），请人工核对。");
        return;
      }
      setActiveDeleteTaskId(undefined);
      setTerminalDeleteTaskId(undefined);
      setDeleteOpen(true);
    }, 0);
    return () => window.clearTimeout(timer);
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [autoOpenDelete, pendingSelection, selection.selectedIds.length]);

  return (
    <section aria-label="重复组工作台">
      <fieldset
        aria-label="重复组交互控件"
        className="groups-page groups-page__controls"
        disabled={deleteLocked}
        inert={isDrawer && selectedGroupId !== undefined}
      >
        <GroupFilters
          agents={agents}
          density={density}
          kind={kind}
          machine={machine}
          minMembers={minMembersInput}
          onDensityChange={setDensity}
          onKindChange={changeKind}
          onMachineChange={value => changeFilter(setMachine, value)}
          onMinMembersChange={changeMinMembers}
          onQueryChange={changeSearch}
          onSortChange={value => changeFilter(setSort, value)}
          query={searchInput}
          sort={sort}
        />
        <section className="groups-page__results">
          {agentState.error
            ? <p className="groups-page__agent-warning" role="alert">
                Agent 状态不可用，已将在线集合视为空并清除不可验证的删除选择。
              </p>
            : null}
          {groups.error
            ? <div className="groups-page__list-error">
                <p role="alert">{groups.error.message}</p>
                <button onClick={groups.reload} type="button">重试重复组列表</button>
              </div>
            : null}
          {groups.loading && !groups.data ? <p role="status">正在加载重复组…</p> : null}
          {groups.loading && groups.data ? <p className="groups-page__refreshing" role="status">正在刷新列表，当前显示上次成功结果。</p> : null}
          <div className="groups-page__results-toolbar">
            <button
              className="groups-page__refresh-button"
              onClick={refreshGroupsAndStats}
              type="button"
            >
              刷新列表
            </button>
            {!isMobile
              ? <button
                  className="groups-page__refresh-button"
                  disabled={agentState.error !== undefined}
                  onClick={() => { setBatchError(undefined); setBatchSelectOpen(true); }}
                  title={agentState.error !== undefined ? "Agent 状态不可用，无法安全批量选择" : "对当前筛选命中的所有组应用保留策略"}
                  type="button"
                >
                  批量选择
                </button>
              : null}
            {filterStats !== undefined && filterStats.query === filterStatsQuery
              ? <span className="groups-page__filter-stats">当前筛选共 {filterStats.stats.groups} 组，可回收 {byteText(filterStats.stats.wastedBytes)}</span>
              : null}
          </div>
          <GroupTable density={density} onOpenGroup={openGroup} onPageChange={setPage} onSelectOthers={isMobile ? undefined : selectOthersInGroup} page={groups.data} selectedGroupId={selectedGroupId} />
          {!isMobile
            ? <div className="groups-page__selection">
                <span>{`已选 ${selection.selectedIds.length} 项`}</span>
                {selection.selectedIds.length > 0
                  ? <span>{`共 ${byteText(selectionTotals.totalSize)}，涉及 ${selectionTotals.machineCount} 台设备`}</span>
                  : null}
                {selectionNotice ? <span role="status">{selectionNotice}</span> : null}
                {deleteExecutionPending ? <span role="status">等待删除任务受理</span> : null}
                <button
                  disabled={deleteExecutionPending || (!activeDeleteTaskId && selection.selectedIds.length === 0)}
                  onClick={requestDelete}
                  type="button"
                >
                  {deleteExecutionPending
                    ? "等待删除任务受理"
                    : activeDeleteTaskId
                    ? (terminalDeleteTaskId === activeDeleteTaskId ? "查看删除任务结果" : "查看进行中的删除任务")
                    : `删除已选 ${selection.selectedIds.length} 项`}
                </button>
              </div>
            : null}
        </section>
        <GroupDetail
          detail={detail}
          error={detailError}
          interactionLocked={deleteLocked}
          isDrawer={isDrawer}
          loading={detailLoading}
          onAutoSelect={applyAutoSelect}
          onClose={closeDetail}
          onDelete={requestDelete}
          onPageChange={setMemberPage}
          onRefresh={() => setDetailReload(value => value + 1)}
          onSelectAll={selectAllCurrentPage}
          onSetRepresentative={setRepresentative}
          onToggle={toggleMember}
          open={selectedGroupId !== undefined}
          representativeError={representativeError}
          representativeNotice={representativeNotice}
          representativePending={representativePending}
          selectable={!isMobile}
          selectedIds={selection.selectedSet}
          selectedCount={selection.selectedIds.length}
          unavailableMachineIds={unavailableMachineIds}
        />
      </fieldset>
      <DeleteDialog
        api={api}
        initialTaskId={activeDeleteTaskId}
        keptMembers={keptMembers}
        memberIds={selection.selectedIds}
        onAccepted={acceptDelete}
        onClose={closeDelete}
        onExecutionRejected={() => setDeleteReviewSnapshot(undefined)}
        onExecutionLockChange={setDeleteLocked}
        onExecutionPendingChange={setDeleteExecutionPending}
        onExecutionStarted={startDeleteExecution}
        onTerminal={finishDelete}
        open={deleteOpen}
      />
      <Modal
        onClose={() => { if (!batchPending) setBatchSelectOpen(false); }}
        open={batchSelectOpen}
        title="批量选择"
      >
        <p>对当前筛选命中的所有重复组应用保留策略，勾选各组其余成员；代表文件与 Agent 离线成员由后端始终排除。</p>
        <label className="groups-page__batch-field" htmlFor="batch-strategy">保留策略</label>
        <select
          id="batch-strategy"
          onChange={event => setBatchStrategy(event.target.value as GroupSelectStrategy)}
          value={batchStrategy}
        >
          {GROUP_SELECT_STRATEGY_OPTIONS.map(option =>
            <option key={option.value} value={option.value}>{option.label}</option>)}
        </select>
        {batchError ? <p role="alert">{batchError}</p> : null}
        {batchPending ? <p role="status">正在计算批量选择…</p> : null}
        <div className="groups-page__batch-actions">
          <button disabled={batchPending} onClick={() => setBatchSelectOpen(false)} type="button">取消</button>
          <button disabled={batchPending} onClick={() => void applyBatchSelection()} type="button">应用策略</button>
        </div>
      </Modal>
    </section>
  );
}
