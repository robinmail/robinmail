// Phase 6 in the browser: changes arrive over push (the event source of
// RFC 8620 7.3) instead of the half-minute timer. New mail shows up without
// a reload, a broken connection is made again with growing pauses, and
// without push the timer still does the work.
import { expect, test, type Page } from '@playwright/test'
import { deliver, destroyBySubject, login, textMessage } from './dev'

const row = (page: Page, subject: string) => page.locator('[data-row]', { hasText: subject })
const EVENTS = '**/jmap/eventsource/**'

// Well under the 30 seconds of the timer: what arrives this fast came
// over push. The server finds a change made by another process (the
// command line that delivers) by its poll, once a second.
const SOON = 8000

test('new mail and changes made elsewhere appear without a reload', async ({ page }) => {
  const streams: string[] = []
  page.on('request', (r) => {
    if (r.url().includes('/jmap/eventsource/')) streams.push(r.url())
  })
  await login(page)
  await expect.poll(() => streams.length).toBe(1)
  expect(new URL(streams[0]!).search).toBe('?types=*&closeafter=no&ping=30')

  const subject = `E2E push ${Date.now()}`
  await expect(row(page, subject)).toHaveCount(0)
  const title = await page.title()
  deliver(textMessage(subject, 1))
  await expect(row(page, subject)).toBeVisible({ timeout: SOON })
  await expect(row(page, subject)).toHaveAttribute('data-unread', 'true')
  // The unread count in the title went up with it.
  await expect.poll(() => page.title(), { timeout: SOON }).not.toBe(title)

  // A second one, a little later: the connection is still the first.
  const second = `${subject} again`
  deliver(textMessage(second, 2))
  await expect(row(page, second)).toBeVisible({ timeout: SOON })
  expect(streams).toHaveLength(1)

  // Another client removes them (JMAP with this session, but not through
  // the page's store): the rows go away.
  await destroyBySubject(page, subject)
  await expect(row(page, subject)).toHaveCount(0, { timeout: SOON })
  await expect(row(page, second)).toHaveCount(0, { timeout: SOON })
  await expect.poll(() => page.title(), { timeout: SOON }).toBe(title)
})

test('a lost push connection is made again, with a pause that grows', async ({ page }) => {
  // The first three connections fail; the fourth gets through.
  const attempts: number[] = []
  await page.route(EVENTS, async (route) => {
    attempts.push(Date.now())
    if (attempts.length <= 3) await route.abort('connectionfailed')
    else await route.continue()
  })
  await login(page)
  await expect.poll(() => attempts.length, { timeout: 15_000 }).toBe(4)
  // 1, 2 and 4 seconds, each 75% to 100% of that.
  const gaps = attempts.slice(1).map((t, i) => t - attempts[i]!)
  expect(gaps[0]).toBeGreaterThanOrEqual(700)
  expect(gaps[0]).toBeLessThan(2500)
  expect(gaps[1]).toBeGreaterThanOrEqual(1400)
  expect(gaps[1]).toBeLessThan(3800)
  expect(gaps[2]).toBeGreaterThanOrEqual(2900)
  expect(gaps[2]).toBeLessThan(6000)

  // And it works: mail arrives over the connection that came back.
  const subject = `E2E push reconnect ${Date.now()}`
  deliver(textMessage(subject, 1))
  await expect(row(page, subject)).toBeVisible({ timeout: SOON })
  expect(attempts).toHaveLength(4)
  await destroyBySubject(page, subject)
})

test('without push the timer still finds new mail', async ({ page }) => {
  await page.clock.install()
  let attempts = 0
  await page.route(EVENTS, async (route) => {
    attempts++
    await route.abort('connectionfailed')
  })
  await login(page)
  await expect.poll(() => attempts).toBeGreaterThan(0)

  const subject = `E2E push fallback ${Date.now()}`
  deliver(textMessage(subject, 1))
  // Nothing tells the page. The clock stands still, so no timer runs.
  await page.waitForTimeout(3000)
  await expect(row(page, subject)).toHaveCount(0)
  // Half a minute later the timer asks.
  await page.clock.fastForward(31_000)
  await expect(row(page, subject)).toBeVisible({ timeout: SOON })
  await destroyBySubject(page, subject)
})
