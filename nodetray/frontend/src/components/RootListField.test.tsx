import '@testing-library/jest-dom/vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { vi } from 'vitest'
import { RootListField } from './RootListField'

describe('RootListField', () => {
  it('只接受注入回调返回的非空本地绝对路径，并支持逐项移除', async () => {
    const chooseRoot = vi.fn()
      .mockResolvedValueOnce('relative-folder')
      .mockResolvedValueOnce('')
      .mockResolvedValueOnce('C:\\fixtures\\media-b')
    const onChange = vi.fn()
    const user = userEvent.setup()
    const { rerender } = render(
      <RootListField
        id="allowed-roots"
        name="allowedRoots"
        label="允许的媒体根目录"
        values={['C:\\fixtures\\media-a']}
        chooseRoot={chooseRoot}
        onChange={onChange}
      />,
    )

    const choose = screen.getByRole('button', { name: '选择允许的媒体根目录' })
    await user.click(choose)
    await user.click(choose)
    expect(onChange).not.toHaveBeenCalled()
    await user.click(choose)
    expect(onChange).toHaveBeenCalledWith(['C:\\fixtures\\media-a', 'C:\\fixtures\\media-b'])

    rerender(
      <RootListField
        id="allowed-roots"
        name="allowedRoots"
        label="允许的媒体根目录"
        values={['C:\\fixtures\\media-a', 'C:\\fixtures\\media-b']}
        chooseRoot={chooseRoot}
        onChange={onChange}
      />,
    )
    await user.click(screen.getByRole('button', { name: '移除 C:\\fixtures\\media-a' }))
    expect(onChange).toHaveBeenLastCalledWith(['C:\\fixtures\\media-b'])
  })

  it('没有目录选择回调时可手动添加本地绝对路径，并拒绝空值和相对路径', async () => {
    const onChange = vi.fn()
    const user = userEvent.setup()
    render(
      <RootListField
        id="allowed-roots"
        name="allowedRoots"
        label="允许的媒体根目录"
        values={[]}
        onChange={onChange}
      />,
    )

    const input = screen.getByLabelText('手动输入允许的媒体根目录')
    const add = screen.getByRole('button', { name: '添加允许的媒体根目录' })
    expect(add).toBeEnabled()
    await user.click(add)
    expect(onChange).not.toHaveBeenCalled()
    await user.type(input, 'relative-folder')
    await user.click(add)
    expect(onChange).not.toHaveBeenCalled()
    await user.clear(input)
    await user.type(input, 'C:\\fixtures\\media-a')
    await user.click(add)
    expect(onChange).toHaveBeenCalledOnce()
    expect(onChange).toHaveBeenCalledWith(['C:\\fixtures\\media-a'])
  })

  it('把列表错误与逐项错误定位到对应根目录', () => {
    render(
      <RootListField
        id="allowed-roots"
        name="allowedRoots"
        label="允许的媒体根目录"
        values={['C:\\fixtures\\media-a', 'C:\\fixtures\\media-b']}
        errors={{
          allowedRoots: '至少配置一个允许根目录',
          'allowedRoots[0]': '该目录属于系统目录',
          'allowedRoots[1]': '该目录与另一根目录重叠',
        }}
        onChange={() => undefined}
      />,
    )

    expect(screen.getByText('至少配置一个允许根目录')).toHaveAttribute('role', 'alert')
    const first = screen.getByRole('listitem', { name: 'C:\\fixtures\\media-a' })
    const second = screen.getByRole('listitem', { name: 'C:\\fixtures\\media-b' })
    expect(within(first).getByText('该目录属于系统目录')).toHaveAttribute('role', 'alert')
    expect(within(second).getByText('该目录与另一根目录重叠')).toHaveAttribute('role', 'alert')
  })
})
