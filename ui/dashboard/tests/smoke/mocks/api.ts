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

  // Asset HTTP API requests in prod/preview hit http://<host>:8090/api/v1/<path>,
  // while dev/auth requests stay on the same origin via the SvelteKit proxy
  // mounted at /api/<path>. We anchor with /api/ to avoid matching the SPA's
  // own page routes (e.g. /peers, /settings) and use a wildcard segment for v1.
  await page.route('**/api/**/blockchain/info', (route: Route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(blockchainInfo) }),
  )
  await page.route('**/api/blockchain/info', (route: Route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(blockchainInfo) }),
  )

  await page.route('**/api/**/blockstats**', (route: Route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(blockstats) }),
  )
  await page.route('**/api/blockstats**', (route: Route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(blockstats) }),
  )

  await page.route('**/api/**/peers**', (route: Route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(peers) }),
  )
  await page.route('**/api/peers**', (route: Route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(peers) }),
  )

  await page.route('**/api/**/settings**', (route: Route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(settings) }),
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

  // /ancestors expects either { block_locator: string[] } or a bare array.
  // Production URL is /api/v1/block_locator; dev proxy mounts at
  // /api/blockchain/locator. Match both.
  const locatorBody = JSON.stringify({ block_locator: [] })
  await page.route('**/api/**/block_locator', (route: Route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: locatorBody }),
  )
  await page.route('**/api/blockchain/locator', (route: Route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: locatorBody }),
  )

  // /admin reads { events: [...] } from /api/v1/fsm/events on the asset host.
  await page.route('**/api/**/fsm/events', (route: Route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ events: [] }),
    }),
  )
}
