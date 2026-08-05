import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

function removeRemoteReactDiagnostics() {
  const reactErrorAddress = ['https:', '', 'react.dev', 'errors', ''].join('/')
  return {
    name: 'remove-remote-react-diagnostics',
    enforce: 'post' as const,
    generateBundle(_options: unknown, bundle: Record<string, { type: string; code?: string }>) {
      for (const output of Object.values(bundle)) {
        if (output.type === 'chunk' && output.code !== undefined) {
          output.code = output.code.replaceAll(reactErrorAddress, 'react-error-')
        }
      }
    },
  }
}

export default defineConfig({
  plugins: [react(), removeRemoteReactDiagnostics()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    sourcemap: false,
  },
})
