import { VirtualTable } from "../../components/VirtualTable";
import type { GroupPage, GroupSummary } from "../../api/contracts";

export interface GroupTableProps {
  readonly density: "compact" | "comfortable";
  readonly onOpenGroup: (id: number) => void;
  readonly onPageChange: (page: number) => void;
  readonly page: GroupPage | undefined;
  readonly selectedGroupId: number | undefined;
}

function byteText(value: number): string {
  if (value < 1024) return `${value} B`;
  if (value < 1024 ** 2) return `${(value / 1024).toFixed(1)} KB`;
  if (value < 1024 ** 3) return `${(value / 1024 ** 2).toFixed(1)} MB`;
  return `${(value / 1024 ** 3).toFixed(1)} GB`;
}

function row(summary: GroupSummary, selected: boolean, estimate: number, onOpenGroup: (id: number) => void) {
  return (
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
  );
}

export function GroupTable({ density, onOpenGroup, onPageChange, page, selectedGroupId }: GroupTableProps) {
  const estimate = density === "compact" ? 44 : 56;
  const currentPage = page?.page ?? 1;
  const total = page?.total ?? 0;
  const pageSize = page?.size ?? 100;
  const lastPage = Math.max(1, Math.ceil(total / pageSize));
  return (
    <section aria-label="重复组结果" className="group-table" data-row-estimate={estimate} data-row-height={estimate}>
      <div className="group-table__meta">
        <span>{`行高估算：${estimate}px`}</span>
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
            renderRow={summary => row(summary, selectedGroupId === summary.id, estimate, onOpenGroup)}
            rowKey={summary => summary.id}
          />}
      <nav aria-label="重复组分页" className="group-table__pagination">
        <button className="group-table__pager-button" disabled={currentPage <= 1} onClick={() => onPageChange(currentPage - 1)} type="button">上一页</button>
        <span>{`第 ${currentPage} / ${lastPage} 页`}</span>
        <button className="group-table__pager-button" disabled={currentPage >= lastPage} onClick={() => onPageChange(currentPage + 1)} type="button">下一页</button>
      </nav>
    </section>
  );
}
