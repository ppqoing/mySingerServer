import { act, renderHook } from '@testing-library/react'
import { useDirtyForm } from './useDirtyForm'

describe('useDirtyForm', () => {
  it('保留不可变基线并在 reset 后恢复嵌套值', () => {
    const initial = { machineId: 'node-a', database: { password: '', port: 5432 } }
    const { result } = renderHook(() => useDirtyForm(initial))

    initial.database.port = 9999
    act(() => result.current.update((current) => ({
      ...current,
      database: { ...current.database, port: 6432 },
    })))
    expect(result.current.dirty).toBe(true)

    act(() => result.current.reset())
    expect(result.current.value.database.port).toBe(5432)
    expect(result.current.dirty).toBe(false)
  })

  it('commit 将当前值设为新基线且后续编辑仍能识别 dirty', () => {
    const { result } = renderHook(() => useDirtyForm({ count: 1, tags: ['.jpg'] }))

    act(() => result.current.update((current) => ({ ...current, count: 2 })))
    act(() => result.current.commit())
    expect(result.current.value.count).toBe(2)
    expect(result.current.dirty).toBe(false)

    act(() => result.current.update((current) => ({ ...current, tags: [...current.tags, '.png'] })))
    expect(result.current.dirty).toBe(true)
    act(() => result.current.reset())
    expect(result.current.value).toEqual({ count: 2, tags: ['.jpg'] })
  })
})
