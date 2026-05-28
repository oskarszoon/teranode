import type { Page, Route } from '@playwright/test'
import blockchainInfo from './fixtures/blockchain-info.json' with { type: 'json' }
import blockstats from './fixtures/blockstats.json' with { type: 'json' }
import peers from './fixtures/peers.json' with { type: 'json' }
import settings from './fixtures/settings.json' with { type: 'json' }

type MockOptions = {
  authenticated?: boolean
}

const EMPTY_OK = { ok: true, data: [] }

export async function installApiMocks(page: Page, opts: MockOptions = {}): Promise<void> {
  const authenticated = opts.authenticated ?? true

  // Catch-all for /api/* requests so nothing hangs. Registered FIRST so the
  // specific routes below take precedence (Playwright matches handlers LIFO).
  await page.route('**/api/**', (route: Route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(EMPTY_OK) }),
  )

  await page.route('**/api/auth/check', (route: Route) =>
    route.fulfill({
      status: authenticated ? 200 : 401,
      contentType: 'application/json',
      body: JSON.stringify({ authenticated }),
    }),
  )

  await page.route('**/api/blockchain/info', (route: Route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(blockchainInfo) }),
  )

  await page.route('**/api/blockstats**', (route: Route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(blockstats) }),
  )

  await page.route('**/api/peers**', (route: Route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(peers) }),
  )

  await page.route('**/api/settings**', (route: Route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(settings) }),
  )

  await page.route('**/api/config/websocket', (route: Route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ websocketUrl: 'ws://localhost:4173/ws-mock' }),
    }),
  )
}
