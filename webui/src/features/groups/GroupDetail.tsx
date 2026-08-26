import {
  useLayoutEffect,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent
} from "react";
import { createPortal } from "react-dom";
import { Modal } from "../../components/Modal";
import { CopyButton } from "../../components/CopyButton";
import { overlayLayer, registerOverlay, type OverlayHandle } from "../../components/overlayStack";
import { VirtualTable } from "../../components/VirtualTable";
import type { GroupDetail as GroupDetailModel, GroupKind, GroupMember, GroupSelectStrategy } from "../../api/contracts";
import { byteText } from "./format";
import { GROUP_SELECT_STRATEGY_OPTIONS } from "./strategy";

export interface GroupDetailProps {
  readonly detail: GroupDetailModel | undefined;
  readonly error: Error | undefined;
  readonly interactionLocked: boolean;
  readonly isDrawer: boolean;
  readonly loading: boolean;
  readonly open: boolean;
  /** false 时（移动端）整体关闭选择：不渲染成员复选框、全选与删除入口。 */
  readonly selectable: boolean;
  readonly onAutoSelect?: (strategy: GroupSelectStrategy) => void;
  readonly onClose: () => void;
  readonly onDelete?: () => void;
  readonly onPageChange: (page: number) => void;
  readonly onRefresh: () => void;
  readonly onSelectAll: (select: boolean) => void;
  readonly onSetRepresentative?: (member: GroupMember) => void;
  readonly onToggle: (fileId: number) => void;
  readonly representativeError?: string;
  readonly representativeNotice?: string;
  readonly representativePending?: boolean;
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

// 64 位感知哈希的汉明距离分档阈值：≤5 几乎相同，≤10 轻微差异，更大为中度差异。
function hammingLevel(distance: number): string {
  if (distance <= 5) return "极小";
  if (distance <= 10) return "小";
  return "中";
}

/**
 * 已知组类型的 score 结构化解读：exact 为 sha512 全等，image/video 含
 * hamming（+video 的 duration_diff_ms）。peer_sha512、quality_* 等内部字段不展示。
 * 形状不匹配时返回 undefined，由调用方回退到防御性格式化。
 */
function readableScore(kind: GroupKind, score: Record<string, unknown>): string | undefined {
  if (kind === "exact") {
    return score.basis === "sha512" ? "内容完全一致" : undefined;
  }
  const distance = score.hamming ?? score.dist;
  if (typeof distance !== "number" || !Number.isFinite(distance)) return undefined;
  const rounded = Math.max(0, Math.round(distance));
  const parts = [`与代表文件的差异：距离 ${rounded}（${hammingLevel(rounded)}）`];
  const durationDiff = score.duration_diff_ms;
  if (kind === "video" && typeof durationDiff === "number" && Number.isFinite(durationDiff)) {
    parts.push(`时长差 ${(Math.abs(durationDiff) / 1000).toFixed(1)} 秒`);
  }
  return parts.join("；");
}

function textScore(kind: GroupKind, value: unknown): string {
  if (value === null || value === undefined) return "—";
  if (typeof value === "object" && !Array.isArray(value)) {
    const readable = readableScore(kind, value as Record<string, unknown>);
    if (readable !== undefined) return readable;
  }
  // 未知结构的兜底：有界深度/长度 + WeakSet 环检测的逐键格式化。
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

/** 预览代理由 gui.exe 按 machine+fileId 桥接到 agent 本机通道；视频组不发请求。 */
function memberPreviewURL(member: GroupMember, size: number): string {
  const params = new URLSearchParams({ machine: member.machineId, w: String(size), h: String(size) });
  return `/api/files/${member.fileId}/preview?${params}`;
}

interface MemberThumbnailProps {
  readonly compareDisabled: boolean;
  readonly kind: GroupKind;
  readonly member: GroupMember;
  readonly onCompare: (member: GroupMember) => void;
}

function MemberThumbnail({ compareDisabled, kind, member, onCompare }: MemberThumbnailProps) {
  const [failed, setFailed] = useState(false);
  return (
    <button
      aria-label={`对比文件 ${member.fileId} 与代表文件`}
      className="group-detail__member-thumbnail-button"
      disabled={compareDisabled}
      onClick={() => onCompare(member)}
      title={compareDisabled ? "代表文件自身无需对比" : "与代表文件并排对比"}
      type="button"
    >
      {kind === "video"
        ? <span className="group-detail__member-thumbnail group-detail__member-thumbnail--video" data-testid="member-thumbnail-placeholder">视频预览暂不支持</span>
        : failed
          ? <span aria-hidden="true" className="group-detail__member-thumbnail" data-testid="member-thumbnail-placeholder" />
          : <img
              alt={`文件 ${member.fileId} 预览`}
              className="group-detail__member-thumbnail-image"
              loading="lazy"
              onError={() => setFailed(true)}
              src={memberPreviewURL(member, 320)}
            />}
    </button>
  );
}

function memberRow(
  member: GroupMember,
  detail: GroupDetailModel,
  selectedIds: ReadonlySet<number>,
  unavailableMachineIds: ReadonlySet<string>,
  selectable: boolean,
  onToggle: (fileId: number) => void,
  onInspect: (member: GroupMember) => void,
  onCompare: (member: GroupMember) => void,
  onSetRepresentative: ((member: GroupMember) => void) | undefined,
  representativePending: boolean
) {
  const representative = member.fileId === detail.representativeFileId;
  const offline = unavailableMachineIds.has(member.machineId);
  const canSelect = selectable && eligible(member, detail, unavailableMachineIds);
  return (
    <article className="group-detail__member" data-row-height={MEMBER_ROW_HEIGHT}>
      <div className="group-detail__member-heading">
        <MemberThumbnail
          compareDisabled={representative}
          kind={detail.kind}
          member={member}
          onCompare={onCompare}
        />
        {selectable
          ? <label>
              <input
                aria-label={`选择文件 ${member.fileId}`}
                checked={selectedIds.has(member.fileId)}
                disabled={!canSelect}
                onChange={() => onToggle(member.fileId)}
                type="checkbox"
              />
              <span>文件 #{member.fileId}</span>
            </label>
          : <span className="group-detail__member-name">文件 #{member.fileId}</span>}
        <span className="group-detail__member-badges">
          {representative ? <span className="group-detail__badge">代表文件</span> : null}
          {offline ? <span className="group-detail__badge group-detail__badge--offline">Agent 离线</span> : null}
        </span>
        <div className="group-detail__member-actions">
          {onSetRepresentative && !representative && !offline
            ? <button
                aria-label={`将文件 ${member.fileId} 设为保留副本`}
                className="group-detail__action-button group-detail__keep-button"
                disabled={representativePending}
                onClick={() => onSetRepresentative(member)}
                title="将该成员设为组的保留副本（代表文件永不被删除）"
                type="button"
              >
                设为保留
              </button>
            : null}
          <button
            aria-label={`查看文件 ${member.fileId} 完整信息`}
            className="group-detail__action-button group-detail__inspect-button"
            onClick={() => onInspect(member)}
            type="button"
          >
            查看完整信息
          </button>
        </div>
      </div>
      <dl>
        <dt>Agent</dt><dd>{member.machineId}</dd>
        <dt>路径</dt>
        <dd className="group-detail__member-path">
          <span className="group-detail__member-value--truncated">{member.path}</span>
          <CopyButton label="复制路径" text={member.path} />
        </dd>
        <dt>大小</dt><dd>{byteText(member.size)}</dd>
        <dt>修改时间</dt><dd>{new Date(member.mtime * 1000).toLocaleString("zh-CN")}</dd>
        <dt>相似度</dt><dd className="group-detail__member-value--truncated" data-testid="member-score">{textScore(detail.kind, member.score)}</dd>
      </dl>
    </article>
  );
}

function fullInformation(member: GroupMember, kind: GroupKind) {
  return (
    <dl className="group-detail__full-info-details">
      <dt>Agent</dt><dd>{member.machineId}</dd>
      <dt>完整路径</dt>
      <dd className="group-detail__member-path" data-testid="member-full-path">
        <span>{member.path}</span>
        <CopyButton label="复制路径" text={member.path} />
      </dd>
      <dt>大小</dt><dd>{byteText(member.size)}</dd>
      <dt>修改时间</dt><dd>{new Date(member.mtime * 1000).toLocaleString("zh-CN")}</dd>
      <dt>评分</dt><dd data-testid="member-full-score">{textScore(kind, member.score)}</dd>
    </dl>
  );
}

function compareImage(member: GroupMember, kind: GroupKind) {
  if (kind === "video") {
    return <span className="group-detail__compare-video">视频预览暂不支持</span>;
  }
  return (
    <img
      alt={`文件 ${member.fileId} 预览`}
      className="group-detail__compare-image"
      loading="lazy"
      src={memberPreviewURL(member, 640)}
    />
  );
}

/** 并排对比成员与代表文件；元数据不同的行两格同时高亮。 */
function compareInformation(member: GroupMember, representative: GroupMember | undefined, kind: GroupKind) {
  const rows = [
    { label: "路径", memberValue: member.path, representativeValue: representative?.path },
    { label: "大小", memberValue: byteText(member.size), representativeValue: representative ? byteText(representative.size) : undefined },
    {
      label: "修改时间",
      memberValue: new Date(member.mtime * 1000).toLocaleString("zh-CN"),
      representativeValue: representative ? new Date(representative.mtime * 1000).toLocaleString("zh-CN") : undefined
    },
    { label: "相似度", memberValue: textScore(kind, member.score), representativeValue: representative ? textScore(kind, representative.score) : undefined }
  ];
  return (
    <table className="group-detail__compare-table">
      <thead>
        <tr>
          <th scope="col">属性</th>
          <th scope="col">{`文件 #${member.fileId}`}</th>
          <th scope="col">{representative ? `代表文件 #${representative.fileId}` : "代表文件"}</th>
        </tr>
      </thead>
      <tbody>
        <tr>
          <th scope="row">预览</th>
          <td>{compareImage(member, kind)}</td>
          <td>{representative ? compareImage(representative, kind) : "代表文件不在当前成员页"}</td>
        </tr>
        {rows.map(row => {
          const different = row.representativeValue !== undefined && row.memberValue !== row.representativeValue;
          return (
            <tr key={row.label}>
              <th scope="row">{row.label}</th>
              <td className={different ? "group-detail__compare-value--different" : undefined}>{row.memberValue}</td>
              <td className={different ? "group-detail__compare-value--different" : undefined}>{row.representativeValue ?? "—"}</td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

function content(
  props: GroupDetailProps,
  onInspect: (member: GroupMember) => void,
  onCompare: (member: GroupMember) => void
) {
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
        {props.selectable && props.onAutoSelect
          ? <select
              aria-label="自动选择"
              className="group-detail__auto-select"
              disabled={currentEligible.length === 0}
              onChange={event => {
                if (event.target.value) props.onAutoSelect?.(event.target.value as GroupSelectStrategy);
              }}
              title="按保留策略勾选本组其余成员（代表与离线成员始终排除）"
              value=""
            >
              <option value="">自动选择…</option>
              {GROUP_SELECT_STRATEGY_OPTIONS.map(option =>
                <option key={option.value} value={option.value}>{option.label}</option>)}
            </select>
          : null}
        <button className="group-detail__action-button" onClick={props.onRefresh} type="button">刷新成员</button>
      </div>
      {props.representativeError
        ? <p className="group-detail__action-error" role="alert">{props.representativeError}</p>
        : null}
      {props.representativeNotice ? <p role="status">{props.representativeNotice}</p> : null}
      {props.selectable
        ? <label className="group-detail__select-all">
            <input
              checked={allEligibleSelected}
              disabled={currentEligible.length === 0}
              onChange={event => props.onSelectAll(event.target.checked)}
              type="checkbox"
            />
            全选当前页可删除项
          </label>
        : null}
      {detail.members.length === 0
        ? <p className="group-detail__empty">当前成员页没有文件。</p>
        : <div className="group-detail__members" data-row-height={MEMBER_ROW_HEIGHT}>
            <VirtualTable
              ariaLabel="重复组成员列表"
              estimateSize={() => MEMBER_ROW_HEIGHT}
              items={detail.members}
              key={`${detail.id}-${detail.memberPage}-${detail.members.length}`}
              overscan={8}
              renderRow={member => memberRow(member, detail, props.selectedIds, props.unavailableMachineIds, props.selectable, props.onToggle, onInspect, onCompare, props.onSetRepresentative, props.representativePending ?? false)}
              rowKey={member => member.fileId}
            />
          </div>}
      <nav aria-label="成员分页" className="group-detail__pagination">
        <button className="group-detail__action-button" disabled={detail.memberPage <= 1} onClick={() => props.onPageChange(detail.memberPage - 1)} type="button">上一页成员</button>
        <button className="group-detail__action-button" disabled={detail.memberPage >= lastPage} onClick={() => props.onPageChange(detail.memberPage + 1)} type="button">下一页成员</button>
      </nav>
      {props.isDrawer && props.selectable && props.onDelete ? (
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
  const [comparedMember, setComparedMember] = useState<GroupMember | undefined>(undefined);
  const drawerRef = useRef<HTMLElement>(null);
  const drawerCloseRef = useRef<HTMLButtonElement>(null);
  const drawerOverlayRef = useRef<OverlayHandle | undefined>(undefined);
  const scrimRef = useRef<HTMLButtonElement>(null);

  function closeDetail() {
    if (props.interactionLocked) return;
    setInspectedMember(undefined);
    setComparedMember(undefined);
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
  const currentComparedMember = comparedMember && props.detail?.members.includes(comparedMember)
    ? comparedMember
    : undefined;
  const representativeMember = props.detail?.members.find(member => member.fileId === props.detail?.representativeFileId);
  const body = content(props, setInspectedMember, setComparedMember);
  const inspectionTitle = currentInspectedMember
    ? `文件 ${currentInspectedMember.fileId} 完整信息`
    : "文件完整信息";
  const inspectionModal = (
    <Modal
      onClose={() => setInspectedMember(undefined)}
      open={currentInspectedMember !== undefined}
      title={inspectionTitle}
    >
      {currentInspectedMember && props.detail
        ? fullInformation(currentInspectedMember, props.detail.kind)
        : null}
    </Modal>
  );
  const comparisonTitle = currentComparedMember
    ? `文件 ${currentComparedMember.fileId} 对比`
    : "文件对比";
  const comparisonModal = (
    <Modal
      onClose={() => setComparedMember(undefined)}
      open={currentComparedMember !== undefined}
      title={comparisonTitle}
    >
      {currentComparedMember && props.detail
        ? compareInformation(currentComparedMember, representativeMember, props.detail.kind)
        : null}
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
      {comparisonModal}
    </>
  );
}
