import '@testing-library/jest-dom/vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ExitDialog, type ExitDialogDependencies } from './ExitDialog'

const success = {
  ok: true,
  failedComponents: [] as string[],
  errorCode: '',
  errorSummary: '',
}

function dependencies(result = success): ExitDialogDependencies {
  return { forceExitAll: vi.fn(async () => result) }
}

describe('ExitDialog', () => {
  it('取消时保留所有进程，dirty 明示未保存更改会丢失', async () => {
    const deps = dependencies()
    const onReturn = vi.fn()
    render(<ExitDialog open dirty dependencies={deps} onReturn={onReturn} />)

    expect(screen.getByText(/未保存更改将丢失/)).toBeVisible()
    await userEvent.setup().click(screen.getByRole('button', { name: '取消' }))

    expect(onReturn).toHaveBeenCalledOnce()
    expect(deps.forceExitAll).not.toHaveBeenCalled()
  })

  it('确认后只调用一次 ForceExitAll', async () => {
    const deps = dependencies()
    render(<ExitDialog open dirty={false} dependencies={deps} onReturn={() => undefined} />)

    await userEvent.setup().click(screen.getByRole('button', { name: '强制退出全部后台进程' }))

    await waitFor(() => expect(deps.forceExitAll).toHaveBeenCalledOnce())
  })

  it('后台仍存活时保持弹窗并显示失败组件，可重试同一请求', async () => {
    const forceExitAll = vi
      .fn()
      .mockResolvedValueOnce({
        ok: false,
        failedComponents: ['helper', 'worker:42'],
        errorCode: 'force_exit_timeout',
        errorSummary: '后台进程未全部退出',
      })
      .mockResolvedValueOnce(success)
    render(<ExitDialog open dirty={false} dependencies={{ forceExitAll }} onReturn={() => undefined} />)
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: '强制退出全部后台进程' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('helper、worker:42')
    const retry = screen.getByRole('button', { name: '重试强制退出' })
    expect(retry).toBeEnabled()
    await user.click(retry)
    await waitFor(() => expect(forceExitAll).toHaveBeenCalledTimes(2))
  })
})
