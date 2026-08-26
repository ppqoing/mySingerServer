import { useState } from "react";
import { VirtualTable } from "../../components/VirtualTable";
import type { GroupPage, GroupSummary } from "../../api/contracts";
import { byteText } from "./format";

export interface GroupTableProps {
  readonly density: "compact" | "comfortable";
  readonly onOpenGroup: (id: number) => void;
  readonly onPageChange: (page: number) => void;
  readonly onSelectOthers?: (id: number) => void;
  readonly page: GroupPage | undefined;
  readonly selectedGroupId: number | undefined;
}

function row(
  summary: GroupSummary,
  selected: boolean,
  estimate: number,
  onOpenGroup: (id: number) => void,
  onSelectOthers: ((id: number) => void) | undefined
) {
  return (
    <div className="group-table__row">
      <button
        aria-pressed={selected}
        aria-label={`打开重复组 ${summary.id}`}
        className={`group-table__row-action group-table__row-action--${estimate}`}
        data-row-height={estimate}
        onClick={() => onOpenGroup(summary.id)}
        type="button"
      >
        <span className="group-table__id">#{summary.id}</span>
        <span className="group-table__representative"><b>{summary.repMachine}</b>{summary.repPath}</span>
        <span className="group-table__members">{summary.memberCount} 个文件</span>
        <span className="group-table__machines">{summary.machines.length} 台设备</span>
        <span className="group-table__total">总计 {byteText(summary.totalBytes)}</span>
        <span className="group-table__reclaim">可回收 {byteText(summary.wastedBytes)}</span>
        <time className="group-table__created" dateTime={summary.createdAt}>{new Date(summary.createdAt).toLocaleString("zh-CN")}</time>
      </button>
      {onSelectOthers
        ? <button
            aria-label={`选中重复组 ${summary.id} 的其余成员`}
            className="group-table__select-others"
            onClick={() => onSelectOthers(summary.id)}
            title="保留代表文件，选中该组其余可删除成员（不打开详情）"
            type="button"
          >
            选中其余
          </button>
        : null}
    </div>
  );
}

export function GroupTable({ density, onOpenGroup, onPageChange, onSelectOthers, page, selectedGroupId }: GroupTableProps) {
  const estimate = density === "compact" ? 44 : 56;
  const total = page?.total ?? 0;
  const pageSize = page?.size ?? 100;
  // 加载新页期间保持显示上次真实页码，避免分页器跳回"第 1 / 1 页"造成视觉跳变。
  // 用"渲染期派生状态"模式（与 useSelection 相同），不在渲染期读写 ref。
  const [lastGoodPagination, setLastGoodPagination] = useState({ page: 1, lastPage: 1 });
  if (page !== undefined) {
    const real = { page: page.page, lastPage: Math.max(1, Math.ceil(page.total / page.size)) };
    if (real.page !== lastGoodPagination.page || real.lastPage !== lastGoodPagination.lastPage) {
      setLastGoodPagination(real);
    }
  }
  const currentPage = page?.page ?? lastGoodPagination.page;
  const lastPage = page !== undefined
    ? Math.max(1, Math.ceil(total / pageSize))
    : lastGoodPagination.lastPage;
  const [jumpInput, setJumpInput] = useState("");

  function jumpToPage() {
    const target = Number(jumpInput);
    setJumpInput("");
    if (!Number.isSafeInteger(target) || target < 1) return;
    const clamped = Math.min(target, lastPage);
    if (clamped !== currentPage) onPageChange(clamped);
  }

  return (
    <section aria-label="重复组结果" className="group-table" data-row-estimate={estimate} data-row-height={estimate}>
      <div className="group-table__meta">
        <span>{`本页 ${page?.groups.length ?? 0} 个重复组 / 共 ${total.toLocaleString("en-US")} 个`}</span>
      </div>
      {page && page.groups.length === 0
        ? <p className="group-table__empty">当前筛选没有重复组。</p>
        : <VirtualTable
            ariaLabel="重复组列表"
            estimateSize={() => estimate}
            header={<>
              <div className="group-table__heading group-table__heading--full">
                组 / 代表文件 / 文件数 / Agent / 总容量 / 可回收 / 创建时间
              </div>
              <div className="group-table__heading group-table__heading--compact">
                组 / 代表文件 / 文件数 / 总容量 / 可回收
              </div>
            </>}
            items={page?.groups ?? []}
            key={`${page?.kind ?? "loading"}-${page?.page ?? 0}-${density}`}
            overscan={8}
            renderRow={summary => row(summary, selectedGroupId === summary.id, estimate, onOpenGroup, onSelectOthers)}
            rowKey={summary => summary.id}
          />}
      <nav aria-label="重复组分页" className="group-table__pagination">
        <button className="group-table__pager-button" disabled={currentPage <= 1} onClick={() => onPageChange(1)} type="button">首页</button>
        <button className="group-table__pager-button" disabled={currentPage <= 1} onClick={() => onPageChange(currentPage - 1)} type="button">上一页</button>
        <span>{`第 ${currentPage} / ${lastPage} 页`}</span>
        <button className="group-table__pager-button" disabled={currentPage >= lastPage} onClick={() => onPageChange(currentPage + 1)} type="button">下一页</button>
        <button className="group-table__pager-button" disabled={currentPage >= lastPage} onClick={() => onPageChange(lastPage)} type="button">末页</button>
        <span className="group-table__jump">
          <input
            aria-label="跳转页码"
            className="group-table__jump-input"
            min={1}
            onChange={event => setJumpInput(event.target.value)}
            onKeyDown={event => {
              if (event.key === "Enter") {
                event.preventDefault();
                jumpToPage();
              }
            }}
            placeholder={`1-${lastPage}`}
            type="number"
            value={jumpInput}
          />
          <button className="group-table__pager-button" onClick={jumpToPage} type="button">跳转</button>
        </span>
      </nav>
    </section>
  );
}
