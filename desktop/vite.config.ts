import { readdirSync } from 'node:fs'
import path from 'node:path'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { defineConfig } from 'vitest/config'

/** The themes are files outside desktop/ (`../shared/themes`, shared with Android): a build from a copy of desktop/ alone has none, and the window would start blank. */
const themeFiles = (): import('vite').Plugin => ({
  name: 'agent-mesh-theme-files',
  buildStart() {
    const dir = path.resolve(import.meta.dirname, '../shared/themes')
    let found: string[] = []
    try {
      found = readdirSync(dir).filter((f) => f.endsWith('.json'))
    } catch {
      // Reported below.
    }
    if (found.length === 0) this.error(`no theme files in ${dir}: build from a checkout that has shared/themes beside desktop/`)
  },
})

export default defineConfig({
  base: './',
  plugins: [react(), tailwindcss(), themeFiles()],
  resolve: { alias: { '@': path.resolve(import.meta.dirname, 'src') } },
  build: { outDir: 'dist', emptyOutDir: true, target: 'chrome140' },
  test: {
    environment: 'jsdom',
    setupFiles: ['src/test-setup.ts'],
    include: ['src/**/*.test.{ts,tsx}', 'electron/**/*.test.ts'],
  },
})
