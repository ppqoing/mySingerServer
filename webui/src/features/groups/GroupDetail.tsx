import {
  useLayoutEffect,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent
} from "react";
import { createPortal } from "react-dom";
import { Modal } from "../../components/Modal";
import { overlayLayer, registerOverlay, type OverlayHandle } from "../../components/overlayStack";
import { VirtualTable } from "../../components/VirtualTable";
import type { GroupDetail as GroupDetailModel, GroupMember } from "../../api/contracts";

export interface GroupDetailProps {
  readonly detail: GroupDetailModel | undefined;
  readonly error: Error | undefined;
  readonly interactionLocked: boolean;
  readonly isDrawer: boolean;
  readonly loading: boolean;
  readonly open: boolean;
  readonly onClose: () => void;
  readonly onDelete?: () => void;
  readonly onPageChange: (page: number) => void;
  readonly onRefresh: () => void;
  readonly onSelectAll: (select: boolean) => void;
  readonly onToggle: (fileId: number) => void;
  readonly selectedIds: ReadonlySet<number>;
  readonly selectedCount: number;
  readonly unavailableMachineIds: ReadonlySet<string>;
}

const SCORE_TEXT_LIMIT = 512;
const SCORE_STRING_LIMIT = 128;
const SCORE_KEY_LIMIT = 48;
const SCORE_COLLECTION_LIMIT = 12;
const SCORE_DEPTH_LIMIT = 4;
const MEMBER_ROW_HEIGHT = 208;
const focusableSelector = "a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex='-1'])";

function getFocusable(container: HTMLElement) {
  return Array.from(container.querySelectorAll<HTMLElement>(focusableSelector)).filter(
    element => !element.hasAttribute("hidden") && element.getAttribute("aria-hidden") !== "true"
  );
}

function clip(value: string, limit: number): string {
  return value.length <= limit ? value : `${value.slice(0, Math.max(0, limit - 1))}…`;
}

function compareText(left: string, right: string): number {
  return left < right ? -1 : left > right ? 1 : 0;
}

function boundedSmallestKeys(value: Record<string, unknown>): string[] {
  const keys: string[] = [];
  for (const key in value) {
    if (!Object.prototype.hasOwnProperty.call(value, key)) continue;
    if (keys.length < SCORE_COLLECTION_LIMIT) {
      keys.push(key);
      keys.sort(compareText);
      continue;
    }
    if (compareText(key, keys[keys.length - 1]) < 0) {
      keys[keys.length - 1] = key;
      keys.sort(compareText);
    }
  }
  return keys;
}

function textScore(value: unknown): string {
  const seen = new WeakSet<object>();
  const format = (current: unknown, depth: number): string => {
    if (current === null) return "null";
    if (typeof current === "string") return clip(current, SCORE_STRING_LIMIT);
    if (typeof current !== "object") return clip(String(current), SCORE_STRING_LIMIT);
    if (seen.has(current)) return "[Circular]";
    if (depth >= SCORE_DEPTH_LIMIT) return "…";
    seen.add(current);
    try {
      if (Array.isArray(current)) {
        const values = current.slice(0, SCORE_COLLECTION_LIMIT).map(item => format(item, depth + 1));
        return `[${values.join(", ")}${current.length > SCORE_COLLECTION_LIMIT ? ", …" : ""}]`;
      }
      const record = current as Record<string, unknown>;
      return boundedSmallestKeys(record).map(key => {
        let item: unknown;
        try {
          item = record[key];
        } catch {
          item = "[Unreadable]";
        }
        return `${clip(key, SCORE_KEY_LIMIT)}: ${format(item, depth + 1)}`;
      }).join(", ");
    } finally {
      seen.delete(current);
    }
  };
  return clip(format(value, 0), SCORE_TEXT_LIMIT);
}

function eligible(member: GroupMember, detail: GroupDetailModel, unavailableMachineIds: ReadonlySet<string>): boolean {
  return member.fileId !== detail.representativeFileId && !unavailableMachineIds.has(member.machineId);
}

function memberRow(
  member: GroupMember,
  detail: GroupDetailModel,
  selectedIds: ReadonlySet<number>,
  unavailableMachineIds: ReadonlySet<string>,
  onToggle: (fileId: number) => void,
  onInspect: (member: GroupMember) => void
) {
  const representative = member.fileId === detail.representativeFileId;
  const offline = unavailableMachineIds.has(member.machineId);
  const canSelect = eligible(member, detail, unavailableMachineIds);
  return (
    <article className="group-detail__member" data-row-height={MEMBER_ROW_HEIGHT}>
      <div className="group-detail__member-heading">
        <span
          aria-hidden="true"
          className="group-detail__member-thumbnail"
          data-testid="member-thumbnail-placeholder"
        />
        <label>
          <input
            aria-label={`选择文件 ${member.fileId}`}
            checked={selectedIds.has(member.fileId)}
            disabled={!canSelect}
            onChange={() => onToggle(member.fileId)}
            type="checkbox"
          />
          <span>文件 #{member.fileId}</span>
        </label>
        <span className="group-detail__member-badges">
          {representative ? <span className="group-detail__badge">代表文件</span> : null}
          {offline ? <span className="group-detail__badge group-detail__badge--offline">Agent 离线</span> : null}
        </span>
        <button
          aria-label={`查看文件 ${member.fileId} 完整信息`}
          className="group-detail__action-button group-detail__inspect-button"
          onClick={() => onInspect(member)}
          type="button"
        >
          查看完整信息
        </button>
      </div>
      <dl>
        <dt>Agent</dt><dd>{member.machineId}</dd>
        <dt>路径</dt><dd className="group-detail__member-value--truncated">{member.path}</dd>
        <dt>大小</dt><dd>{member.size.toLocaleString("en-US")} B</dd>
        <dt>修改时间</dt><dd>{new Date(member.mtime * 1000).toLocaleString("zh-CN")}</dd>
        <dt>相似度</dt><dd className="group-detail__member-value--truncated" data-testid="member-score">{textScore(member.score)}</dd>
      </dl>
    </article>
  );
}

function fullInformation(member: GroupMember) {
  return (
    <dl className="group-detail__full-info-details">
      <dt>Agent</dt><dd>{member.machineId}</dd>
      <dt>完整路径</dt><dd data-testid="member-full-path">{member.path}</dd>
      <dt>大小</dt><dd>{member.size.toLocaleString("en-US")} B</dd>
      <dt>修改时间</dt><dd>{new Date(member.mtime * 1000).toLocaleString("zh-CN")}</dd>
      <dt>评分</dt><dd data-testid="member-full-score">{textScore(member.score)}</dd>
    </dl>
  );
}

function content(props: GroupDetailProps, onInspect: (member: GroupMember) => void) {
  if (props.error) {
    return (
      <div className="group-detail__error">
        <p role="alert">{props.error.message}</p>
        <button className="group-detail__action-button" onClick={props.onRefresh} type="button">重试详情</button>
      </div>
    );
  }
  if (!props.detail) {
    if (props.loading || props.open) return <p role="status">正在加载重复组详情…</p>;
    return <p>选择一个重复组以查看文件。</p>;
  }

  const detail = props.detail;
  const lastPage = Math.max(1, Math.ceil(detail.memberTotal / detail.memberSize));
  const currentEligible = detail.members.filter(member => eligible(member, detail, props.unavailableMachineIds));
  const allEligibleSelected = currentEligible.length > 0 && currentEligible.every(member => props.selectedIds.has(member.fileId));
  return (
    <>
      <div className="group-detail__toolbar">
        <span>{`文件 ${detail.memberTotal.toLocaleString("en-US")} 个，第 ${detail.memberPage} / ${lastPage} 页`}</span>
        <button className="group-detail__action-button" onClick={props.onRefresh} type="button">刷新成员</button>
      </div>
      <label className="group-detail__select-all">
        <input
          checked={allEligibleSelected}
          disabled={currentEligible.length === 0}
          onChange={event => props.onSelectAll(event.target.checked)}
          type="checkbox"
        />
        全选当前页可删除项
      </label>
      {detail.members.length === 0
        ? <p className="group-detail__empty">当前成员页没有文件。</p>
        : <div className="group-detail__members" data-row-height={MEMBER_ROW_HEIGHT}>
            <VirtualTable
              ariaLabel="重复组成员列表"
              estimateSize={() => MEMBER_ROW_HEIGHT}
              items={detail.members}
              key={`${detail.id}-${detail.memberPage}-${detail.members.length}`}
              overscan={8}
              renderRow={member => memberRow(member, detail, props.selectedIds, props.unavailableMachineIds, props.onToggle, onInspect)}
              rowKey={member => member.fileId}
            />
          </div>}
      <nav aria-label="成员分页" className="group-detail__pagination">
        <button className="group-detail__action-button" disabled={detail.memberPage <= 1} onClick={() => props.onPageChange(detail.memberPage - 1)} type="button">上一页成员</button>
        <button className="group-detail__action-button" disabled={detail.memberPage >= lastPage} onClick={() => props.onPageChange(detail.memberPage + 1)} type="button">下一页成员</button>
      </nav>
      {props.isDrawer && props.onDelete ? (
        <div className="group-detail__delete-action">
          <button
            className="group-detail__action-button"
            disabled={props.selectedCount === 0}
            onClick={props.onDelete}
            type="button"
          >
            {`删除已选 ${props.selectedCount} 项`}
          </button>
        </div>
      ) : null}
    </>
  );
}

export function GroupDetail(props: GroupDetailProps) {
  const [inspectedMember, setInspectedMember] = useState<GroupMember | undefined>(undefined);
  const drawerRef = useRef<HTMLElement>(null);
  const drawerCloseRef = useRef<HTMLButtonElement>(null);
  const drawerOverlayRef = useRef<OverlayHandle | undefined>(undefined);
  const scrimRef = useRef<HTMLButtonElement>(null);

  function closeDetail() {
    if (props.interactionLocked) return;
    setInspectedMember(undefined);
    props.onClose();
  }

  useLayoutEffect(() => {
    if (!props.isDrawer || !props.open) return;
    const drawer = drawerRef.current;
    const scrim = scrimRef.current;
    if (!drawer || !scrim) return;
    const handle = registerOverlay([scrim, drawer], {
      layer: overlayLayer.drawer,
      restoreFocus: document.activeElement instanceof HTMLElement ? document.activeElement : null
    });
    drawerOverlayRef.current = handle;
    if (handle.isTop()) drawerCloseRef.current?.focus();
    return () => {
      drawerOverlayRef.current = undefined;
      handle.release();
    };
  }, [props.isDrawer, props.open]);

  const currentInspectedMember = inspectedMember && props.detail?.members.includes(inspectedMember)
    ? inspectedMember
    : undefined;
  const body = content(props, setInspectedMember);
  const inspectionTitle = currentInspectedMember
    ? `文件 ${currentInspectedMember.fileId} 完整信息`
    : "文件完整信息";
  const inspectionModal = (
    <Modal
      onClose={() => setInspectedMember(undefined)}
      open={currentInspectedMember !== undefined}
      title={inspectionTitle}
    >
      {currentInspectedMember ? fullInformation(currentInspectedMember) : null}
    </Modal>
  );
  const title = props.open
    ? <div className="group-detail__title"><h2>重复组详情</h2><button className="group-detail__action-button" onClick={closeDetail} ref={drawerCloseRef} type="button">关闭详情</button></div>
    : null;

  const trapDrawerFocus = (event: ReactKeyboardEvent<HTMLElement>) => {
    const drawer = drawerRef.current;
    if (!drawer || !drawerOverlayRef.current?.isTop() || !drawer.contains(event.target as Node)) return;
    if (event.key === "Escape") {
      event.preventDefault();
      closeDetail();
      return;
    }
    if (event.key !== "Tab") return;
    const focusable = getFocusable(drawer);
    if (focusable.length === 0) {
      event.preventDefault();
      drawer.focus();
      return;
    }
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  };

  const detailPanel = props.isDrawer
    ? props.open ? createPortal(
      <>
        <button
          aria-label="关闭详情遮罩"
          className="group-detail__scrim"
          data-testid="group-detail-scrim"
          disabled={props.interactionLocked}
          onClick={closeDetail}
          ref={scrimRef}
          tabIndex={-1}
          type="button"
        />
        <aside
          aria-label="重复组详情"
          aria-modal="true"
          className="group-detail group-detail--drawer"
          onKeyDown={trapDrawerFocus}
          ref={drawerRef}
          role="dialog"
          tabIndex={-1}
          >
          <fieldset className="group-detail__controls" disabled={props.interactionLocked}>
            {title}
            {body}
          </fieldset>
        </aside>
      </>,
      document.body
    ) : null
    : (
    <aside aria-label="重复组详情" className="group-detail">
      <fieldset className="group-detail__controls" disabled={props.interactionLocked}>
        {title}
        {body}
      </fieldset>
    </aside>
  );
  return (
    <>
      {detailPanel}
      {inspectionModal}
    </>
  );
}
