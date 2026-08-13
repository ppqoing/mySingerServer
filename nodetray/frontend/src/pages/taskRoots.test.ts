import { planTaskRootAddition } from './taskRoots'

describe('planTaskRootAddition', () => {
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
