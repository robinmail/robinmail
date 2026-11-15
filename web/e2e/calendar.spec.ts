// Phase 9: the calendar of the Angular webmail. Events go through JMAP for
// Calendars; an invitation between the two users of the instance goes
// through the server's scheduling (iTIP), not by email.
import { expect, type Page, test } from '@playwright/test'
import { OTHER, USER, angular, jmap, login } from './dev'

test.skip(!angular, 'the calendar is in the Angular webmail only')

// removeEvents destroys the events of the page's account whose title
// holds the text.
async function removeEvents(page: Page, title: string): Promise<void> {
  await jmap(page, [
    ['CalendarEvent/query', { filter: { title } }],
    ['CalendarEvent/set', { '#destroy': { resultOf: 'c0', name: 'CalendarEvent/query', path: '/ids' } }],
  ])
}

async function eventsTitled(page: Page, title: string): Promise<Record<string, any>[]> {
  const [, got] = await jmap(page, [
    ['CalendarEvent/query', { filter: { title } }],
    ['CalendarEvent/get', { '#ids': { resultOf: 'c0', name: 'CalendarEvent/query', path: '/ids' } }],
  ])
  return got!.list
}

test('a repeating event: made, seen in a week and a month, one day moved, the series deleted', async ({ page }) => {
  const title = `E2E standup ${Date.now()}`
  await login(page)
  try {
    await page.getByTestId('app-calendar').click()
    await expect(page).toHaveURL(/\/calendar/)
    // Monday 3 March 2031: a week no other test uses.
    await page.goto('/calendar?view=week&date=2031-03-03')
    await expect(page.getByTestId('calendar-title')).toContainText('2031')

    await page.getByTestId('new-event').click()
    await page.getByTestId('event-title').fill(title)
    await page.getByTestId('event-start').fill('2031-03-03T09:00')
    await page.getByTestId('event-end').fill('2031-03-03T09:30')
    await page.getByTestId('event-repeat').selectOption('daily')
    await page.getByTestId('event-count').fill('5')
    await page.getByTestId('event-save').click()
    await expect(page.getByTestId('event-chip').filter({ hasText: title })).toHaveCount(5)

    const [ev] = await eventsTitled(page, title)
    expect(ev).toMatchObject({ title, start: '2031-03-03T09:00:00', duration: 'PT30M', recurrenceRule: { frequency: 'daily', count: 5 } })

    await page.getByTestId('view-month').click()
    await expect(page.getByTestId('calendar-title')).toContainText('2031')
    await expect(page.getByTestId('event-chip').filter({ hasText: title })).toHaveCount(5)

    // Wednesday's alone moves to the afternoon, with a title of its own.
    await page.getByTestId('view-week').click()
    await page.locator('[data-testid="calendar-day"][data-date="2031-03-05"]').getByTestId('event-chip').filter({ hasText: title }).click()
    await expect(page.getByTestId('event-scope')).toHaveValue('one')
    await page.getByTestId('event-title').fill(`${title} moved`)
    await page.getByTestId('event-start').fill('2031-03-05T15:00')
    await page.getByTestId('event-save').click()
    await expect(page.getByTestId('event-chip').filter({ hasText: `${title} moved` })).toHaveCount(1)
    const [after] = await eventsTitled(page, title)
    expect(after!.recurrenceOverrides['2031-03-05T09:00:00']).toMatchObject({ title: `${title} moved`, start: '2031-03-05T15:00:00' })

    // The whole series goes.
    await page.getByTestId('event-chip').filter({ hasText: title }).first().click()
    await page.getByTestId('event-delete').click()
    await page.getByTestId('delete-all').click()
    await expect(page.getByTestId('event-chip').filter({ hasText: title })).toHaveCount(0)
    expect(await eventsTitled(page, title)).toHaveLength(0)
  } finally {
    await removeEvents(page, title)
  }
})

test('an invitation to the other user, accepted in their calendar, shows in the organizer\'s', async ({ page, browser }) => {
  const title = `E2E review ${Date.now()}`
  await login(page)
  const other = await (await browser.newContext()).newPage()
  try {
    await page.goto('/calendar?view=week&date=2031-03-10')
    await page.getByTestId('new-event').click()
    await page.getByTestId('event-title').fill(title)
    await page.getByTestId('event-start').fill('2031-03-11T14:00')
    await page.getByTestId('event-end').fill('2031-03-11T15:00')
    await page.getByTestId('event-guests').fill(OTHER)
    await page.getByTestId('event-save').click()
    await expect(page.getByTestId('event-chip').filter({ hasText: title })).toHaveCount(1)

    await login(other, OTHER)
    await other.goto('/calendar?view=week&date=2031-03-10')
    const invitation = other.getByTestId('invitation').filter({ hasText: title })
    await expect(invitation).toBeVisible()
    await invitation.getByRole('button', { name: 'Accept' }).click()
    await expect(invitation).toHaveCount(0)
    // In their week, with their answer.
    await other.getByTestId('event-chip').filter({ hasText: title }).click()
    await expect(other.getByTestId('my-status')).toHaveText('Going')

    await expect(async () => {
      const [ev] = await eventsTitled(page, title)
      const p = Object.values(ev!.participants as Record<string, any>).find((x) => x.calendarAddress === `mailto:${OTHER}`)
      expect(p?.participationStatus).toBe('accepted')
    }).toPass({ timeout: 10_000 })

    // The organizer cancels: the other's copy says so.
    await page.getByTestId('event-chip').filter({ hasText: title }).click()
    await page.getByTestId('event-delete').click()
    await expect(async () => {
      const [ev] = await eventsTitled(other, title)
      expect(ev?.status).toBe('cancelled')
    }).toPass({ timeout: 10_000 })
  } finally {
    await removeEvents(page, title)
    await removeEvents(other, title)
  }
})

test('import an .ics file', async ({ page }) => {
  const title = `E2E imported ${Date.now()}`
  await login(page)
  try {
    await page.goto('/calendar?view=month&date=2031-04-01')
    const ics = `BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//e2e//EN\r\nBEGIN:VEVENT\r\nUID:${title.replace(/ /g, '-')}@e2e\r\nDTSTAMP:20310101T000000Z\r\n` +
      `DTSTART;VALUE=DATE:20310415\r\nDTEND;VALUE=DATE:20310416\r\nSUMMARY:${title}\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n`
    await page.getByTestId('import-ics').setInputFiles({ name: 'event.ics', mimeType: 'text/calendar', buffer: Buffer.from(ics) })
    await expect(page.getByText('1 event imported')).toBeVisible()
    await expect(page.locator('[data-testid="calendar-day"][data-date="2031-04-15"]').getByTestId('event-chip')).toContainText(title)
    const [ev] = await eventsTitled(page, title)
    expect(ev).toMatchObject({ showWithoutTime: true, start: '2031-04-15T00:00:00', duration: 'P1D' })
  } finally {
    await removeEvents(page, title)
  }
})

test('a calendar of its own, shared with the other user to read', async ({ page, browser }) => {
  const name = `E2E team ${Date.now()}`
  await login(page)
  const other = await (await browser.newContext()).newPage()
  let id = ''
  try {
    await page.goto('/calendar?view=week&date=2031-05-05')
    await page.getByRole('button', { name: 'New calendar' }).click()
    await page.getByTestId('calendar-name').fill(name)
    await page.getByTestId('calendar-save').click()
    const item = page.getByTestId('calendar-item').filter({ hasText: name })
    await expect(item).toBeVisible()
    await item.getByRole('button', { name: `Settings of ${name}` }).click()
    await page.getByTestId('share-email').fill(OTHER)
    await page.getByTestId('share-add').click()
    await expect(page.getByTestId('share-row').filter({ hasText: OTHER })).toBeVisible()
    const [cals] = await jmap(page, [['Calendar/get', {}]])
    id = cals!.list.find((c: any) => c.name === name).id

    await login(other, OTHER)
    await other.goto('/calendar')
    await expect(other.getByTestId('calendar-item').filter({ hasText: name })).toContainText(USER)
  } finally {
    if (id) await jmap(page, [['Calendar/set', { destroy: [id], onDestroyRemoveEvents: true }]])
  }
})
