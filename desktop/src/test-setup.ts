import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'

// The window's layout is kept in localStorage (`layout.ts`): none of it may leak into the next test.
afterEach(() => {
  cleanup()
  if (typeof localStorage !== 'undefined') localStorage.clear()
})
