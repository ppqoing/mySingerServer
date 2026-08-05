import '@testing-library/jest-dom/vitest'

const entryMocks = vi.hoisted(() => ({
  createRoot: vi.fn(),
  render: vi.fn(),
}))

vi.mock('react-dom/client', () => ({
  createRoot: entryMocks.createRoot,
}))

vi.mock('./App', () => ({
  App: () => null,
}))

vi.mock('./components/AppErrorBoundary', () => ({
  AppErrorBoundary: ({ children }: { children?: unknown }) => children,
}))

type RootErrorOptions = {
  onCaughtError?: (error: unknown, errorInfo: unknown) => void
  onRecoverableError?: (error: unknown, errorInfo: unknown) => void
}

describe('production entry error handling', () => {
  it('installs silent React 19 root callbacks that do not expose raw errors', async () => {
    document.body.innerHTML = '<div id="root"></div>'
    entryMocks.createRoot.mockReturnValue({ render: entryMocks.render })
    vi.resetModules()

    await import('./main')

    expect(entryMocks.createRoot).toHaveBeenCalledOnce()
    const options = entryMocks.createRoot.mock.calls[0][1] as RootErrorOptions | undefined
    expect(options?.onCaughtError).toEqual(expect.any(Function))
    expect(options?.onRecoverableError).toEqual(expect.any(Function))

    const consoleSpies = [
      vi.spyOn(console, 'error').mockImplementation(() => undefined),
      vi.spyOn(console, 'warn').mockImplementation(() => undefined),
      vi.spyOn(console, 'log').mockImplementation(() => undefined),
      vi.spyOn(console, 'info').mockImplementation(() => undefined),
    ]
    const rawError = new Error('password=secret')
    const rawInfo = { componentStack: 'D:\\private\\media.tsx:42' }

    options?.onCaughtError?.(rawError, rawInfo)
    options?.onRecoverableError?.(rawError, rawInfo)

    for (const spy of consoleSpies) {
      expect(spy).not.toHaveBeenCalled()
      spy.mockRestore()
    }
  })
})
