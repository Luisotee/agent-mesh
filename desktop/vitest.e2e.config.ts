import { defineConfig } from 'vitest/config'

// The end-to-end tests: the built app, driven over CDP, against a real meshd. Run by `pnpm e2e` (after `pnpm build`).
export default defineConfig({
  test: {
    environment: 'node',
    include: ['e2e/**/*.e2e.ts'],
    testTimeout: 90_000,
    hookTimeout: 120_000,
    fileParallelism: false,
  },
})
