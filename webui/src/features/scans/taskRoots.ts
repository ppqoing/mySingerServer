export type RootChange =
  | { kind: "invalid" }
  | { kind: "duplicate" }
  | { kind: "covered" }
  | { kind: "add"; roots: string[] }
  | { kind: "replace"; roots: string[]; covered: string[] };

export function addTaskRoot(current: readonly string[], candidate: string): RootChange {
  const normalized = normalizeTaskRoot(candidate);
  if (!normalized) return { kind: "invalid" };

  const candidateKey = pathKey(normalized);
  const normalizedCurrent = current.map(root => ({ root, normalized: normalizeTaskRoot(root) })).filter(
    (value): value is { root: string; normalized: string } => value.normalized !== undefined
  );
  if (normalizedCurrent.some(value => pathKey(value.normalized) === candidateKey)) return { kind: "duplicate" };
  if (normalizedCurrent.some(value => containsPath(value.normalized, normalized))) return { kind: "covered" };

  const covered = normalizedCurrent.filter(value => containsPath(normalized, value.normalized)).map(value => value.root);
  if (covered.length > 0) {
    return { kind: "replace", roots: [...current.filter(root => !covered.includes(root)), normalized], covered };
  }
  return { kind: "add", roots: [...current, normalized] };
}

export function normalizeTaskRoot(value: string): string | undefined {
  const path = collapseWindowsSeparators(value.trim().replaceAll("/", "\\"));
  if (!isAbsoluteWindowsPath(path)) return undefined;
  if (/^[a-zA-Z]:\\+$/.test(path)) return `${path.slice(0, 2)}\\`;
  return path.replace(/\\+$/, "");
}

function collapseWindowsSeparators(path: string): string {
  const unc = /^\\{2,}/.test(path);
  const remainder = unc ? path.replace(/^\\+/, "") : path;
  return `${unc ? "\\\\" : ""}${remainder.replace(/\\+/g, "\\")}`;
}

function isAbsoluteWindowsPath(path: string): boolean {
  return /^[a-zA-Z]:\\/.test(path) || /^\\\\[^\\]+\\[^\\]+(?:\\|$)/.test(path);
}

function containsPath(parent: string, child: string): boolean {
  const parentKey = pathKey(parent);
  const childKey = pathKey(child);
  return childKey.startsWith(parentKey.endsWith("\\") ? parentKey : `${parentKey}\\`);
}

function pathKey(path: string): string {
  return path.toLowerCase();
}
