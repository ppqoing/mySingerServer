import { useMemo, useState } from "react";

export interface SelectionState {
  selectedIds: number[];
  selectedSet: ReadonlySet<number>;
  toggle(id: number): void;
  clear(): void;
  replace(ids: readonly number[]): void;
}

export function useSelection(scopeKey: string, protectedIds: ReadonlySet<number>): SelectionState {
  const [store, setStore] = useState(() => ({
    scopeKey,
    protectedIds,
    selected: new Set<number>()
  }));
  if (store.scopeKey !== scopeKey || store.protectedIds !== protectedIds) {
    setStore({
      scopeKey,
      protectedIds,
      selected: store.scopeKey === scopeKey
        ? new Set([...store.selected].filter(id => !protectedIds.has(id)))
        : new Set()
    });
  }
  const selectedIds = useMemo(() => {
    if (store.scopeKey !== scopeKey || store.protectedIds !== protectedIds) {
      return [];
    }
    return [...store.selected].sort((left, right) => left - right);
  }, [protectedIds, scopeKey, store]);
  const selectedSet = useMemo(() => new Set(selectedIds), [selectedIds]);

  return {
    selectedIds,
    selectedSet,
    toggle(id) {
      if (!Number.isSafeInteger(id) || id <= 0 || protectedIds.has(id)) {
        return;
      }
      setStore(current => {
        const next = current.scopeKey === scopeKey ? new Set(current.selected) : new Set<number>();
        if (next.has(id)) {
          next.delete(id);
        } else {
          next.add(id);
        }
        return { scopeKey, protectedIds, selected: next };
      });
    },
    clear() {
      setStore({ scopeKey, protectedIds, selected: new Set() });
    },
    replace(ids) {
      setStore({
        scopeKey,
        protectedIds,
        selected: new Set(ids.filter(id =>
          Number.isSafeInteger(id) && id > 0 && !protectedIds.has(id)
        ))
      });
    }
  };
}
