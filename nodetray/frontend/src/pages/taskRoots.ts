export type TaskRootPlan = {
  kind: 'added' | 'duplicate' | 'covered' | 'replace-children' | 'invalid'
  roots: string[]
  coveredRoots: string[]
}

function normalizeTaskRoot(value: string): string {
  const normalized = value.trim().replaceAll('/', '\\')
  if (!normalized) return ''
  if (/^[a-zA-Z]:\\+$/.test(normalized)) return `${normalized.slice(0, 1).toUpperCase()}:\\`
  return normalized.replace(/\\+$/, '')
}

function key(value: string): string { return normalizeTaskRoot(value).toLocaleLowerCase() }

function isParent(parent: string, child: string): boolean {
  const parentKey = key(parent)
  const childKey = key(child)
  return parentKey !== childKey && childKey.startsWith(parentKey.endsWith('\\') ? parentKey : `${parentKey}\\`)
}

export function planTaskRootAddition(roots: string[], value: string): TaskRootPlan {
  const candidate = normalizeTaskRoot(value)
  if (!candidate) return { kind: 'invalid', roots, coveredRoots: [] }
  if (roots.some((root) => key(root) === key(candidate))) return { kind: 'duplicate', roots, coveredRoots: [] }

  const parent = roots.find((root) => isParent(root, candidate))
  if (parent) return { kind: 'covered', roots, coveredRoots: [parent] }

  const children = roots.filter((root) => isParent(candidate, root))
  if (children.length) return { kind: 'replace-children', roots: [...roots.filter((root) => !children.includes(root)), candidate], coveredRoots: children }
  return { kind: 'added', roots: [...roots, candidate], coveredRoots: [] }
}
