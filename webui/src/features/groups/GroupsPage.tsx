import { useEffect, useMemo, useRef, useState } from "react";
import { appApi } from "../../api/appApi";
import { isAbortError } from "../../api/client";
import type {
  AgentStatus,
  AppApi,
  DeleteTaskStatus,
  GroupDetail as GroupDetailModel,
  GroupKind,
  GroupQuery,
  GroupSort
} from "../../api/contracts";
import { usePagedGroups } from "../../hooks/usePagedGroups";
import { usePolling } from "../../hooks/usePolling";
import { useSelection } from "../../hooks/useSelection";
import { DeleteDialog } from "../deletion/DeleteDialog";
import {
  deriveDeleteRetryPlan,
  type DeleteReviewMember,
  type DeleteReviewSnapshot
} from "./deleteReview";
import { GroupDetail } from "./GroupDetail";
import { GroupFilters } from "./GroupFilters";
import { GroupTable } from "./GroupTable";
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
  onRequestDelete
}: GroupsPageProps) {
  const [kind, setKind] = useState<GroupKind>("exact");
  const [page, setPage] = useState(1);
  const [searchInput, setSearchInput] = useState("");
  const [debouncedSearch, setDebouncedSearch] = useState("");
  const [machine, setMachine] = useState("");
  const [minMembers, setMinMembers] = useState("");
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

  const query = useMemo(
    () => currentQuery(kind, page, debouncedSearch, machine, minMembers, sort),
    [debouncedSearch, kind, machine, minMembers, page, sort]
  );
  const groups = usePagedGroups(api, query);
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
  const selectionScope = selectedGroupId === undefined ? `${kind}:none` : `${kind}:${selectedGroupId}`;
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
          path: candidate.path
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

  function openGroup(id: number) {
    const nextScope = `${kind}:${id}`;
    if (nextScope !== selectionScope) resetSelectionOwners(nextScope);
    setSelectionNotice(undefined);
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

  function closeDetail() {
    setSelectedGroupId(undefined);
    setMemberPage(1);
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
    const members = memberIds.map(fileId =>
      activeSelectionOwners.get(fileId) ??
      detail?.members.find(candidate => candidate.fileId === fileId) ??
      { fileId, machineId: "", path: "" }
    ).map(candidate => ({
      fileId: candidate.fileId,
      machineId: candidate.machineId,
      path: candidate.path
    }));
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

  function reconcileDeleteResult(
    status: DeleteTaskStatus,
    snapshot: DeleteReviewSnapshot
  ): boolean {
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
    groups.reload();
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
      setSelectionNotice("删除任务已完成；当前视图没有可核对的原始选择快照。");
      return;
    }
    if (snapshot.scopeKey !== selectionScope) {
      setDeleteReviewSnapshot({ ...snapshot, terminalStatus: status, reconciled: false });
      setSelectionNotice("另一重复组的删除任务已完成；当前选择保持不变。");
      return;
    }
    if (!agentVerificationReady) {
      setDeleteReviewSnapshot({ ...snapshot, terminalStatus: status, reconciled: false });
      setSelectionNotice("删除结果已保留；等待 Agent 状态核验后恢复可重试项。");
      return;
    }
    const reconciled = reconcileDeleteResult(status, snapshot);
    setDeleteReviewSnapshot({ ...snapshot, terminalStatus: status, reconciled });
  }

  useEffect(() => {
    const snapshot = deleteReviewSnapshot;
    if (!snapshot?.terminalStatus || snapshot.reconciled ||
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
  }, [deleteReviewSnapshot, selectedGroupId, selection.selectedIds.length]);

  useEffect(() => {
    const snapshot = deleteReviewSnapshot;
    if (!snapshot?.terminalStatus || !agentVerificationReady ||
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
          minMembers={minMembers}
          onDensityChange={setDensity}
          onKindChange={changeKind}
          onMachineChange={value => changeFilter(setMachine, value)}
          onMinMembersChange={value => changeFilter(setMinMembers, value)}
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
          <GroupTable density={density} onOpenGroup={openGroup} onPageChange={setPage} page={groups.data} selectedGroupId={selectedGroupId} />
          <div className="groups-page__selection">
            <span>{`已选 ${selection.selectedIds.length} 项`}</span>
            {selectionNotice ? <span role="status">{selectionNotice}</span> : null}
            {deleteExecutionPending ? <span role="status">等待删除任务受理</span> : null}
            {!isMobile
              ? <button
                  disabled={deleteExecutionPending || (!activeDeleteTaskId && selection.selectedIds.length === 0)}
                  onClick={requestDelete}
                  type="button"
                >
                  {deleteExecutionPending
                    ? "等待删除任务受理"
                    : activeDeleteTaskId
                    ? "查看进行中的删除任务"
                    : `删除已选 ${selection.selectedIds.length} 项`}
                </button>
              : null}
          </div>
        </section>
        <GroupDetail
          detail={detail}
          error={detailError}
          interactionLocked={deleteLocked}
          isDrawer={isDrawer}
          loading={detailLoading}
          onClose={closeDetail}
          onDelete={isMobile ? undefined : requestDelete}
          onPageChange={setMemberPage}
          onRefresh={() => setDetailReload(value => value + 1)}
          onSelectAll={selectAllCurrentPage}
          onToggle={toggleMember}
          open={selectedGroupId !== undefined}
          selectedIds={selection.selectedSet}
          selectedCount={selection.selectedIds.length}
          unavailableMachineIds={unavailableMachineIds}
        />
      </fieldset>
      <DeleteDialog
        api={api}
        initialTaskId={activeDeleteTaskId}
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
    </section>
  );
}
