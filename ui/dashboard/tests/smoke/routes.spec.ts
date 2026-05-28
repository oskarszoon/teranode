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
