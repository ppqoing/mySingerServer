export const overlayLayer = {
  drawer: 10,
  modal: 20
} as const;

export interface OverlayHandle {
  readonly isTop: () => boolean;
  readonly release: () => void;
}

interface OverlayEntry {
  readonly document: Document;
  readonly layer: number;
  readonly restoreFocusCandidates: readonly HTMLElement[];
  readonly roots: readonly HTMLElement[];
  readonly sequence: number;
  released: boolean;
}

interface ElementSnapshot {
  readonly ariaHidden: string | null;
  readonly inert: string | null;
}

const entries: OverlayEntry[] = [];
const elementSnapshots = new Map<HTMLElement, ElementSnapshot>();
let bodyOverflow: string | undefined;
let nextSequence = 0;

export function getOverlayStackDiagnosticsForTests() {
  return {
    entries: entries.length,
    snapshots: elementSnapshots.size
  };
}

function topEntry(): OverlayEntry | undefined {
  let top: OverlayEntry | undefined;
  for (const entry of entries) {
    if (entry.released) continue;
    if (!top || entry.layer > top.layer ||
      (entry.layer === top.layer && entry.sequence > top.sequence)) {
      top = entry;
    }
  }
  return top;
}

function bodyChildFor(root: HTMLElement, document: Document): HTMLElement | undefined {
  let candidate: HTMLElement | null = root;
  while (candidate.parentElement && candidate.parentElement !== document.body) {
    candidate = candidate.parentElement;
  }
  return candidate.parentElement === document.body ? candidate : undefined;
}

function snapshot(element: HTMLElement) {
  if (elementSnapshots.has(element)) return;
  elementSnapshots.set(element, {
    ariaHidden: element.getAttribute("aria-hidden"),
    inert: element.getAttribute("inert")
  });
}

function restoreAttribute(element: HTMLElement, name: string, value: string | null) {
  if (value === null) {
    element.removeAttribute(name);
  } else {
    element.setAttribute(name, value);
  }
}

function restoreElement(element: HTMLElement) {
  const prior = elementSnapshots.get(element);
  if (!prior) return;
  restoreAttribute(element, "aria-hidden", prior.ariaHidden);
  restoreAttribute(element, "inert", prior.inert);
}

function hideElement(element: HTMLElement) {
  snapshot(element);
  element.setAttribute("inert", "");
  element.setAttribute("aria-hidden", "true");
}

function isolateForTop(document: Document) {
  const top = topEntry();
  if (!top) return;
  const activeRoots = new Set(
    top.roots
      .map(root => bodyChildFor(root, document))
      .filter((root): root is HTMLElement => root !== undefined)
  );
  for (const child of Array.from(document.body.children)) {
    const element = child as HTMLElement;
    snapshot(element);
    if (activeRoots.has(element)) {
      restoreElement(element);
    } else {
      hideElement(element);
    }
  }
  for (const entry of entries) {
    for (const root of entry.roots) {
      if (!root.isConnected) continue;
      if (entry === top) {
        restoreElement(root);
      } else {
        hideElement(root);
      }
    }
  }
}

function restoreDocument(document: Document) {
  for (const [element] of elementSnapshots) {
    if (element.ownerDocument === document && element.isConnected) {
      restoreElement(element);
    }
  }
  elementSnapshots.clear();
  if (bodyOverflow !== undefined) {
    document.body.style.overflow = bodyOverflow;
    bodyOverflow = undefined;
  }
}

function pruneDisconnectedSnapshots(document: Document) {
  for (const element of elementSnapshots.keys()) {
    if (element.ownerDocument === document && !element.isConnected) {
      elementSnapshots.delete(element);
    }
  }
}

function targetBelongsToTop(target: HTMLElement, top: OverlayEntry | undefined) {
  if (!top) return true;
  return top.roots.some(root => root.isConnected && root.contains(target));
}

function restoreConnectedFocus(candidates: readonly HTMLElement[], document: Document) {
  if (candidates.length === 0) return;
  queueMicrotask(() => {
    const top = topEntry();
    for (const target of candidates) {
      if (
        target.ownerDocument === document &&
        target.isConnected &&
        targetBelongsToTop(target, top) &&
        !target.closest("[inert], [aria-hidden='true']")
      ) {
        target.focus();
        return;
      }
    }
  });
}

export function registerOverlay(
  roots: readonly HTMLElement[],
  options: {
    readonly layer: number;
    readonly restoreFocus: HTMLElement | null;
  }
): OverlayHandle {
  const uniqueRoots = [...new Set(roots)];
  if (uniqueRoots.length === 0) {
    throw new Error("overlay requires at least one root");
  }
  const document = uniqueRoots[0].ownerDocument;
  if (uniqueRoots.some(root => root.ownerDocument !== document)) {
    throw new Error("overlay roots must share one document");
  }
  const previousTop = topEntry();
  if (entries.length === 0) {
    elementSnapshots.clear();
    bodyOverflow = document.body.style.overflow;
  }
  const inheritedFocusCandidates = previousTop?.document === document
    ? previousTop.restoreFocusCandidates
    : [];
  const restoreFocusCandidates = [...new Set([
    ...(options.restoreFocus ? [options.restoreFocus] : []),
    ...inheritedFocusCandidates
  ])];
  const entry: OverlayEntry = {
    document,
    layer: options.layer,
    restoreFocusCandidates,
    roots: uniqueRoots,
    sequence: nextSequence++,
    released: false
  };
  entries.push(entry);
  document.body.style.overflow = "hidden";
  isolateForTop(document);

  return {
    isTop: () => !entry.released && topEntry() === entry,
    release: () => {
      if (entry.released) return;
      const wasTop = topEntry() === entry;
      entry.released = true;
      const index = entries.indexOf(entry);
      if (index >= 0) entries.splice(index, 1);
      if (entries.length === 0) {
        restoreDocument(document);
      } else {
        document.body.style.overflow = "hidden";
        isolateForTop(document);
        queueMicrotask(() => pruneDisconnectedSnapshots(document));
      }
      if (wasTop) restoreConnectedFocus(entry.restoreFocusCandidates, document);
    }
  };
}
