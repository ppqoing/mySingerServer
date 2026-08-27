import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { AppApi, FilesystemEntry, FilesystemPage } from "../../api/contracts";
import { Modal } from "../../components/Modal";

export interface RemotePathBrowserProps {
  readonly machineID: string;
  readonly api: AppApi;
  readonly open: boolean;
  readonly onAdd: (path: string) => void;
  readonly onClose: () => void;
}

const pageSize = 100;

export function RemotePathBrowser({ machineID, api, open, onAdd, onClose }: RemotePathBrowserProps) {
  const [showHidden, setShowHidden] = useState(false);
  if (!open) return null;

  return <RemotePathBrowserSession
    api={api}
    key={machineID}
    machineID={machineID}
    onAdd={onAdd}
    onClose={onClose}
    onShowHiddenChange={setShowHidden}
    showHidden={showHidden}
  />;
}

function RemotePathBrowserSession({ machineID, api, onAdd, onClose, onShowHiddenChange, showHidden }: {
  readonly machineID: string;
  readonly api: AppApi;
  readonly onAdd: (path: string) => void;
  readonly onClose: () => void;
  readonly onShowHiddenChange: (showHidden: boolean) => void;
  readonly showHidden: boolean;
}) {
  const [currentPath, setCurrentPath] = useState("");
  const [parentPath, setParentPath] = useState("");
  const [selectedPath, setSelectedPath] = useState("");
  const [entries, setEntries] = useState<FilesystemEntry[]>([]);
  const [nextCursor, setNextCursor] = useState("");
  const [initialShowHidden] = useState(showHidden);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string>();
  const controller = useRef<AbortController | undefined>(undefined);
  const lastAttemptedPath = useRef("");

  const applyPage = useCallback((result: FilesystemPage, append: boolean) => {
    setCurrentPath(result.currentPath);
    setParentPath(result.parentPath);
    setEntries(previous => append ? [...previous, ...result.entries] : result.entries);
    setNextCursor(result.nextCursor);
  }, []);

  const performBrowse = useCallback(async (
    request: AbortController,
    path: string,
    cursor: string,
    append: boolean,
    hidden: boolean
  ) => {
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

  const browse = useCallback((path: string, cursor: string, append: boolean, hidden: boolean) => {
    controller.current?.abort();
    const request = new AbortController();
    controller.current = request;
    setLoading(true);
    setError(undefined);
    return performBrowse(request, path, cursor, append, hidden);
  }, [performBrowse]);

  useEffect(() => {
    const request = new AbortController();
    controller.current = request;
    lastAttemptedPath.current = "";
    void api.browseAgentFilesystem(machineID, {
      path: "", showHidden: initialShowHidden, cursor: "", limit: pageSize
    }, request.signal).then(result => {
      if (!request.signal.aborted) applyPage(result, false);
    }).catch(cause => {
      if (!request.signal.aborted) setError(cause instanceof Error ? cause.message : "无法浏览远程目录。");
    }).finally(() => {
      if (controller.current === request) setLoading(false);
    });
    return () => controller.current?.abort();
  }, [api, applyPage, initialShowHidden, machineID]);

  const breadcrumbPaths = useMemo(() => windowsBreadcrumbs(currentPath), [currentPath]);

  const navigate = async (path: string) => {
    if (await browse(path, "", false, showHidden)) setSelectedPath(path);
  };
  // 重试从失败时目标路径的首页重新加载（不以 append 续页，避免重复条目）。
  const retry = () => {
    void browse(lastAttemptedPath.current, "", false, showHidden);
  };
  const addCurrent = () => {
    const path = selectedPath || currentPath;
    if (path) onAdd(path);
  };

  return (
    <Modal onClose={onClose} open={true} title="选择目录">
      <div className="remote-path-browser">
        <nav aria-label="目录面包屑" className="remote-path-browser__breadcrumbs">
          <button disabled={!parentPath} onClick={() => void navigate(parentPath)} type="button">返回上一级</button>
          <button onClick={() => navigate("")} type="button">计算机</button>
          {breadcrumbPaths.map(crumb => <button key={crumb.path} onClick={() => navigate(crumb.path)} type="button">{crumb.label}</button>)}
        </nav>
        <label className="remote-path-browser__toggle">
          <input checked={showHidden} onChange={event => {
            const hidden = event.target.checked;
            onShowHiddenChange(hidden);
            void browse(currentPath, "", false, hidden);
          }} type="checkbox" />显示隐藏和系统项目
        </label>
        <p className="remote-path-browser__selection">{selectedPath || currentPath}</p>
        {error ? <p role="alert">{error} <button onClick={retry} type="button">重试</button></p> : null}
        <div aria-label="远程目录内容" className="remote-path-browser__entries">
          {loading && entries.length === 0 ? <p role="status">正在加载目录…</p> : null}
          {entries.map(entry => <EntryButton entry={entry} key={entry.path} onNavigate={navigate} selected={selectedPath === entry.path} />)}
        </div>
        <footer className="remote-path-browser__actions">
          {nextCursor ? <button disabled={loading} onClick={() => void browse(currentPath, nextCursor, true, showHidden)} type="button">加载更多</button> : null}
          <button disabled={!selectedPath && !currentPath} onClick={addCurrent} type="button">添加当前目录</button>
          {loading && entries.length > 0 ? <span role="status">正在加载目录…</span> : null}
        </footer>
      </div>
    </Modal>
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
  // 规范化与 taskRoots.normalizeTaskRoot 同源：UNC 根为 \\server\share，盘符根为 X:\。
  const unc = /^(\\\\[^\\]+\\[^\\]+)(?:\\(.*))?$/.exec(path);
  if (unc) {
    const crumbs = [{ label: unc[1], path: unc[1] }];
    let current = unc[1];
    for (const part of (unc[2] ?? "").split("\\").filter(Boolean)) {
      current += `\\${part}`;
      crumbs.push({ label: part, path: current });
    }
    return crumbs;
  }
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
