import { planTaskRootAddition } from './taskRoots'

describe('planTaskRootAddition', () => {

  // Break caught: relative or drive-relative manual text reaches task creation
  // and is interpreted against a different working directory by the backend.
  it('accepts only absolute Windows drive or UNC share paths for manual roots', () => {
    const cases = [
      { value: 'foo', kind: 'invalid' },
      { value: '.\\media', kind: 'invalid' },
      { value: '\\relative', kind: 'invalid' },
      { value: 'C:\\Media', kind: 'added' },
      { value: '\\\\server\\share\\Media', kind: 'added' },
    ] as const

    for (const testCase of cases) {
      expect(planTaskRootAddition([], testCase.value)).toMatchObject({ kind: testCase.kind, roots: testCase.kind === 'added' ? [testCase.value] : [] })
    }
  })
  it('deduplicates Windows paths regardless of case, separators, or trailing slashes', () => {
    expect(planTaskRootAddition(['D:\\Media'], 'd:/media/')).toEqual({
      kind: 'duplicate', roots: ['D:\\Media'], coveredRoots: [],
    })
  })

  it('keeps an existing parent root instead of adding a descendant twice', () => {
    expect(planTaskRootAddition(['D:\\Media'], 'D:\\Media\\Photos')).toEqual({
      kind: 'covered', roots: ['D:\\Media'], coveredRoots: ['D:\\Media'],
    })
  })

  it('plans a confirmation before a new parent replaces selected children', () => {
    expect(planTaskRootAddition(['D:\\Media\\Photos', 'E:\\Photos'], 'D:\\Media')).toEqual({
      kind: 'replace-children', roots: ['E:\\Photos', 'D:\\Media'], coveredRoots: ['D:\\Media\\Photos'],
    })
  })

  it('applies the same boundary rules to UNC shares without crossing a share-name prefix', () => {
    expect(planTaskRootAddition(['\\\\Server\\Share\\Photos'], '\\\\server\\share')).toEqual({
      kind: 'replace-children', roots: ['\\\\server\\share'], coveredRoots: ['\\\\Server\\Share\\Photos'],
    })
    expect(planTaskRootAddition(['\\\\server\\share'], '\\\\server\\share2')).toEqual({
      kind: 'added', roots: ['\\\\server\\share', '\\\\server\\share2'], coveredRoots: [],
    })
  })
})
