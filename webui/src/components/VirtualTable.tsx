import { useVirtualizer } from "@tanstack/react-virtual";
import { useRef, type ReactNode } from "react";

export interface VirtualTableProps<T> {
  readonly ariaLabel: string;
  readonly estimateSize: (index: number) => number;
  readonly header?: ReactNode;
  readonly items: readonly T[];
  readonly overscan?: number;
  readonly renderRow: (item: T, index: number) => ReactNode;
  readonly rowKey: (item: T, index: number) => string | number;
}

export function VirtualTable<T>({ ariaLabel, estimateSize, header, items, overscan = 5, renderRow, rowKey }: VirtualTableProps<T>) {
  const scrollRef = useRef<HTMLDivElement>(null);
  // TanStack Virtual intentionally owns imperative measurement for the scroll surface.
  // eslint-disable-next-line react-hooks/incompatible-library
  const virtualizer = useVirtualizer({
    count: items.length,
    estimateSize,
    getItemKey: (index) => rowKey(items[index], index),
    getScrollElement: () => scrollRef.current,
    initialRect: { height: 512, width: 0 },
    overscan
  });

  return (
    <section className="virtual-table">
      {header ? <div className="virtual-table__header">{header}</div> : null}
      <div aria-label={ariaLabel} className="virtual-table__scroll" ref={scrollRef} role="list">
        <div style={{ height: `${virtualizer.getTotalSize()}px`, position: "relative" }}>
          {virtualizer.getVirtualItems().map((virtualRow) => (
            <div
              className="virtual-table__row"
              data-index={virtualRow.index}
              key={virtualRow.key}
              role="listitem"
              style={{ position: "absolute", top: 0, transform: `translateY(${virtualRow.start}px)`, width: "100%" }}
            >
              {renderRow(items[virtualRow.index], virtualRow.index)}
            </div>
          ))}
        </div>
      </div>
    </section>
  );
}
