/// <reference types="vite/client" />

import viteConfig from '../vite.config'
import indexHtml from '../index.html?raw'

const productionSources = import.meta.glob(
  ['./**/*.{ts,tsx,css}', '!./**/*.test.{ts,tsx}'],
  { eager: true, import: 'default', query: '?raw' },
) as Record<string, string>

const distFiles = import.meta.glob(
  '../dist/**/*.{html,js,css,map}',
  { eager: true, import: 'default', query: '?raw' },
) as Record<string, string>

const requiredCsp = [
  "default-src 'self'",
  "script-src 'self'",
  "style-src 'self'",
  "img-src 'self' data:",
  "font-src 'self'",
  "connect-src 'self'",
  "object-src 'none'",
  "frame-src 'none'",
  "base-uri 'none'",
]

const forbiddenProductionPatterns = [
  ['远程 URL', /https?:\/\//i],
  ['动态代码', /\beval\s*\(|\bnew\s+Function\b/],
  ['开发服务器', /\b(?:localhost|127\.0\.0\.1)(?::\d+)?\b/i],
  ['浏览器持久化', /\b(?:localStorage|sessionStorage|indexedDB)\b/],
  ['HTML 注入', /\b(?:dangerouslySetInnerHTML|innerHTML)\b/],
  ['source map 引用', /sourceMappingURL|\.map(?:[?#"']|$)/i],
] as const

const forbiddenDistPatterns = forbiddenProductionPatterns.filter(([label]) => label !== 'HTML 注入')
const nonFetchingDomNamespaces = [
  'http://www.w3.org/1998/Math/MathML',
  'http://www.w3.org/1999/xlink',
  'http://www.w3.org/2000/svg',
  'http://www.w3.org/XML/1998/namespace',
]

function violations(
  files: Record<string, string>,
  patterns: ReadonlyArray<readonly [string, RegExp]> = forbiddenProductionPatterns,
  allowedLiterals: readonly string[] = [],
): string[] {
  const found: string[] = []
  for (const [path, content] of Object.entries(files)) {
    const scanned = allowedLiterals.reduce((value, literal) => value.replaceAll(literal, ''), content)
    for (const [label, pattern] of patterns) {
      if (pattern.test(scanned)) {
        found.push(`${label}: ${path}`)
      }
    }
    if (/console\.(?:log|info|warn|error)\s*\([^)]*\b(?:password|dsn|secret|token)\b/i.test(scanned)) {
      found.push(`敏感信息日志: ${path}`)
    }
  }
  return found
}

describe('embedded frontend security', () => {
  it('CSP 精确覆盖内嵌本地资源且禁用对象、框架与 base', () => {
    const document = new DOMParser().parseFromString(indexHtml, 'text/html')
    const csp = document.querySelector('meta[http-equiv="Content-Security-Policy"]')?.getAttribute('content') ?? ''
    const directives = csp.split(';').map((value) => value.trim()).filter(Boolean)

    expect(directives).toEqual(requiredCsp)
  })

  it('生产源码不包含远程资源、动态代码、浏览器持久化或 HTML 注入', () => {
    const files = { ...productionSources, '../index.html': indexHtml }
    expect(violations(files)).toEqual([])
  })

  it('Vite 发布构建清空输出目录且关闭 source map', () => {
    if (typeof viteConfig === 'function' || Array.isArray(viteConfig)) {
      throw new Error('vite_config_not_static')
    }
    expect(viteConfig.build?.emptyOutDir).toBe(true)
    expect(viteConfig.build?.sourcemap).toBe(false)
  })

  it('现有 dist 不含远程资源、开发地址、动态代码、持久化或 source map', () => {
    expect(Object.keys(distFiles).length).toBeGreaterThan(0)
    expect(Object.keys(distFiles).filter((path) => path.endsWith('.map'))).toEqual([])
    expect(violations(distFiles, forbiddenDistPatterns, nonFetchingDomNamespaces)).toEqual([])
  })
})
