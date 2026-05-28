import { test, expect } from './fixtures'

test.describe('smoke: /home', () => {
  test('renders with no console errors', async ({ smokePage, consoleErrors }) => {
    const response = await smokePage.goto('/home')
    expect(response?.ok(), 'route should respond 2xx').toBe(true)

    // Page-specific selector. /home has no native <h1>, so the route root
    // carries data-testid="page-root" as a stable hook.
    await expect(smokePage.locator('[data-testid="page-root"]')).toBeVisible()

    await smokePage.waitForTimeout(1000) // idle window for late console output
    expect(consoleErrors, `console errors:\n${consoleErrors.join('\n')}`).toHaveLength(0)
  })
})

type Route = {
  path: string
  selector: string // visible after hydration
  authenticated?: boolean // default true
}

const ROUTES: Route[] = [
  { path: '/', selector: '[data-testid="page-root"]' },
  { path: '/network', selector: '[data-testid="page-root"]' },
  { path: '/p2p', selector: '[data-testid="page-root"]' },
  { path: '/peers', selector: '[data-testid="page-root"]' },
  { path: '/forks', selector: '[data-testid="page-root"]' },
  { path: '/ancestors', selector: '[data-testid="page-root"]' },
  { path: '/viewer', selector: '[data-testid="page-root"]' },
  // /api is a server-only route group (only +server.ts handlers, no +page.svelte),
  // not a navigable page. Smoke does not exercise it.
  { path: '/settings', selector: '[data-testid="page-root"]' },
  { path: '/admin', selector: '[data-testid="page-root"]' },
  { path: '/wstest', selector: '[data-testid="page-root"]' },
  { path: '/login', selector: '[data-testid="page-root"]', authenticated: false },
]

for (const route of ROUTES) {
  test.describe(`smoke: ${route.path}`, () => {
    test.use({ authenticated: route.authenticated ?? true })

    test('renders with no console errors', async ({ smokePage, consoleErrors }) => {
      const response = await smokePage.goto(route.path)
      expect(response?.ok(), `${route.path} should respond 2xx`).toBe(true)
      await expect(smokePage.locator(route.selector)).toBeVisible()
      await smokePage.waitForTimeout(1000)
      expect(consoleErrors, `console errors on ${route.path}:\n${consoleErrors.join('\n')}`).toHaveLength(0)
    })
  })
}

// Click-through smoke: /viewer renders a block list, clicking a hash navigates
// to /viewer/block/?hash=<hash> and the detail page renders without errors.
// Catches regressions in:
//   - blocks-list fetch + table population
//   - block-detail page mount + 3-endpoint compose (block + header + lastblocks)
//   - URL/router behaviour for the /viewer/[type] dynamic route
test.describe('smoke: /viewer click-through to block detail', () => {
  test('opens block detail when first block hash is clicked', async ({ smokePage, consoleErrors }) => {
    await smokePage.goto('/viewer')
    await expect(smokePage.locator('[data-testid="page-root"]')).toBeVisible()

    // First block hash link in the blocks table.
    const firstBlockLink = smokePage.locator('a[href^="/viewer/block/?hash="]').first()
    await expect(firstBlockLink).toBeVisible()
    await firstBlockLink.click()

    await smokePage.waitForURL(/\/viewer\/block\/\?hash=[a-fA-F0-9]+/)

    // Detail page is wrapped in its own data-testid="page-root".
    await expect(smokePage.locator('[data-testid="page-root"]')).toBeVisible()

    await smokePage.waitForTimeout(1000)
    expect(consoleErrors, `console errors on /viewer click-through:\n${consoleErrors.join('\n')}`).toHaveLength(0)
  })
})
