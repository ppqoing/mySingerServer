import { useState } from "react";
import { CopyButton } from "../../components/CopyButton";

export interface RecycledToListProps {
  readonly machineId: string;
  readonly recycledTo: Record<string, string>;
}

const COLLAPSED_LIMIT = 3;

/** 长路径中间缩略展示，完整值放在 title 与复制内容里。 */
function shortenPath(path: string): string {
  return path.length > 48 ? `${path.slice(0, 24)}…${path.slice(-20)}` : path;
}

/** 软删除回收去向明细（源路径 → 去向）；超过 3 条默认折叠。 */
export function RecycledToList({ machineId, recycledTo }: RecycledToListProps) {
  const [expanded, setExpanded] = useState(false);
  const entries = Object.entries(recycledTo).sort(([left], [right]) => left.localeCompare(right));
  if (entries.length === 0) return null;
  const visible = expanded ? entries : entries.slice(0, COLLAPSED_LIMIT);
  return (
    <section aria-label={`Agent ${machineId} 已移入回收目录`} className="recycled-to">
      <p>Agent {machineId} 已移入回收目录：{entries.length} 项</p>
      <ul>
        {visible.map(([source, destination]) => (
          <li key={source}>
            <span title={source}>{shortenPath(source)}</span>
            {" → "}
            <span title={destination}>{shortenPath(destination)}</span>
            {" "}
            <CopyButton label="复制去向" text={destination} />
          </li>
        ))}
      </ul>
      {entries.length > COLLAPSED_LIMIT
        ? <button onClick={() => setExpanded(current => !current)} type="button">
            {expanded ? "收起回收去向" : `展开全部 ${entries.length} 条`}
          </button>
        : null}
    </section>
  );
}
