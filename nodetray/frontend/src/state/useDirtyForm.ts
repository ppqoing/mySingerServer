import { useCallback, useMemo, useState } from 'react'

type ValueUpdater<T> = T | ((current: T) => T)

export type DirtyForm<T> = {
  value: T
  dirty: boolean
  update: (updater: ValueUpdater<T>) => void
  reset: () => void
  commit: (value?: T) => void
}

export function useDirtyForm<T>(initialValue: T): DirtyForm<T> {
  const [baseline, setBaseline] = useState<T>(() => cloneValue(initialValue))
  const [value, setValue] = useState<T>(() => cloneValue(initialValue))

  const update = useCallback((updater: ValueUpdater<T>) => {
    setValue((current) => cloneValue(
      typeof updater === 'function'
        ? (updater as (current: T) => T)(cloneValue(current))
        : updater,
    ))
  }, [])

  const reset = useCallback(() => {
    setValue(cloneValue(baseline))
  }, [baseline])

  const commit = useCallback((nextValue?: T) => {
    setValue((current) => {
      const next = cloneValue(nextValue ?? current)
      setBaseline(cloneValue(next))
      return next
    })
  }, [])

  const dirty = useMemo(() => !deepEqual(value, baseline), [value, baseline])
  return { value, dirty, update, reset, commit }
}

function cloneValue<T>(value: T): T {
  if (Array.isArray(value)) {
    return value.map((item) => cloneValue(item)) as T
  }
  if (value !== null && typeof value === 'object') {
    const clone: Record<string, unknown> = {}
    for (const [key, child] of Object.entries(value as Record<string, unknown>)) {
      clone[key] = cloneValue(child)
    }
    return clone as T
  }
  return value
}

function deepEqual(left: unknown, right: unknown): boolean {
  if (Object.is(left, right)) {
    return true
  }
  if (Array.isArray(left) || Array.isArray(right)) {
    return Array.isArray(left)
      && Array.isArray(right)
      && left.length === right.length
      && left.every((item, index) => deepEqual(item, right[index]))
  }
  if (left !== null && right !== null && typeof left === 'object' && typeof right === 'object') {
    const leftEntries = Object.entries(left)
    const rightEntries = Object.entries(right)
    return leftEntries.length === rightEntries.length
      && leftEntries.every(([key, value]) => deepEqual(value, (right as Record<string, unknown>)[key]))
  }
  return false
}
