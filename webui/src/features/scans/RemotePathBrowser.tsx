import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { AppApi, FilesystemEntry, FilesystemPage } from "../../api/contracts";

export interface RemotePathBrowserProps {
  readonly machineID: string;
  readonly api: AppApi;
  readonly open: boolean;
  readonly onAdd: (path: string) => void;
  readonly onClose: () => void;
}

const pageSize = 100;

export function RemotePathBrowser({ machineID, api, open, onAdd, onClose }: RemotePathBrowserProps) {
  const [currentPath, setCurrentPath] = useState("");
  const [selectedPath, setSelectedPath] = useState("");
  const [entries, setEntries] = useState<FilesystemEntry[]>([]);
  const [nextCursor, setNextCursor] = useState("");
  const [showHidden, setShowHidden] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string>();
  const controller = useRef<AbortController | undefined>(undefined);

  const applyPage = useCallback((result: FilesystemPage, append: boolean) => {
    setCurrentPath(result.currentPath);
    setEntries(previous => append ? [...previous, ...result.entries] : result.entries);
    setNextCursor(result.nextCursor);
  }, []);

  const browse = useCallback(async (path: string, cursor: string, append: boolean, hidden: boolean) => {
    controller.current?.abort();
    const request = new AbortController();
    controller.current = request;
    setLoading(true);
    setError(undefined);
    try {
      const result = await api.browseAgentFilesystem(machineID, {
        path, showHidden: hidden, cursor, limit: pageSize
      }, request.signal);
      if (request.signal.aborted) return false;
      applyPage(result, append);
      return true;
    } catch (cause) {
      if (!request.signal.aborted) setError(cause instanceof Error ? cause.message : "无法浏览远程目录。");
      return false;
    } finally {
      if (controller.current === request) setLoading(false);
    }
  }, [api, applyPage, machineID]);

  useEffect(() => {
    if (!open) {
      controller.current?.abort();
      return;
    }
    setSelectedPath("");
    void browse("", "", false, showHidden);
    return () => controller.current?.abort();
  }, [browse, machineID, open]);

  const breadcrumbPaths = useMemo(() => windowsBreadcrumbs(currentPath), [currentPath]);
  if (!open) return null;

  const navigate = async (path: string) => {
    if (await browse(path, "", false, showHidden)) setSelectedPath(path);
  };
  const addCurrent = () => {
    const path = selectedPath || currentPath;
    if (path) onAdd(path);
  };

  return (
    <div aria-label="选择远程目录" className="remote-path-browser__backdrop" role="dialog" aria-modal="true">
      <section className="remote-path-browser">
        <header className="remote-path-browser__header">
          <h2>选择目录</h2>
          <button onClick={onClose} type="button">关闭</button>
        </header>
        <nav aria-label="目录面包屑" className="remote-path-browser__breadcrumbs">
          <button onClick={() => navigate("")} type="button">计算机</button>
          {breadcrumbPaths.map(crumb => <button key={crumb.path} onClick={() => navigate(crumb.path)} type="button">{crumb.label}</button>)}
        </nav>
        <label className="remote-path-browser__toggle">
          <input checked={showHidden} onChange={event => {
            const hidden = event.target.checked;
            setShowHidden(hidden);
            void browse(currentPath, "", false, hidden);
          }} type="checkbox" />显示隐藏和系统项目
        </label>
        <p className="remote-path-browser__selection">{selectedPath || currentPath}</p>
        {error ? <p role="alert">{error}</p> : null}
        <div aria-label="远程目录内容" className="remote-path-browser__entries">
          {entries.map(entry => <EntryButton entry={entry} key={entry.path} onNavigate={navigate} selected={selectedPath === entry.path} />)}
        </div>
        <footer className="remote-path-browser__actions">
          {nextCursor ? <button disabled={loading} onClick={() => void browse(currentPath, nextCursor, true, showHidden)} type="button">加载更多</button> : null}
          <button disabled={!selectedPath && !currentPath} onClick={addCurrent} type="button">添加当前目录</button>
          {loading ? <span role="status">正在加载目录…</span> : null}
        </footer>
      </section>
    </div>
  );
}

function EntryButton({ entry, onNavigate, selected }: {
  entry: FilesystemEntry;
  onNavigate: (path: string) => void;
  selected: boolean;
}) {
  const disabled = entry.kind === "file" || !entry.selectable;
  return <button
    aria-disabled={disabled ? "true" : undefined}
    aria-pressed={selected}
    className={selected ? "remote-path-browser__entry remote-path-browser__entry--selected" : "remote-path-browser__entry"}
    disabled={disabled}
    onClick={() => onNavigate(entry.path)}
    type="button"
  >{entry.name}</button>;
}

function windowsBreadcrumbs(path: string): Array<{ label: string; path: string }> {
  const drive = /^([a-zA-Z]:\\)(.*)$/.exec(path);
  if (!drive) return [];
  const crumbs = [{ label: drive[1].slice(0, 2), path: drive[1] }];
  let current = drive[1];
  for (const part of drive[2].split("\\").filter(Boolean)) {
    current += part;
    crumbs.push({ label: part, path: current });
    current += "\\";
  }
  return crumbs;
}
