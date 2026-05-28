import type { Page, Route } from '@playwright/test'
import blockchainInfo from './fixtures/blockchain-info.json'
import blockstats from './fixtures/blockstats.json'
import peers from './fixtures/peers.json'
import settings from './fixtures/settings.json'

type MockOptions = {
  authenticated?: boolean
}

const EMPTY_OK = { ok: true, data: [] }

export async function installApiMocks(page: Page, opts: MockOptions = {}): Promise<void> {
  const authenticated = opts.authenticated ?? true

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

  await page.route('**/wsconfig', (route: Route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ url: 'ws://localhost:4173/ws-mock' }),
    }),
  )

  // Catch-all for any other /api/* request so nothing hangs.
  await page.route('**/api/**', (route: Route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(EMPTY_OK) }),
  )
}
