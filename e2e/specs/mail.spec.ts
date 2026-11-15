// The browser half of phase 3's exit: log in, read a message with an HTML
// body, flag it, move it, delete it, and see the counts follow. It runs
// against a real robinmail (see playwright.config.ts) and leaves the
// account as it found it.
import { expect, test, type Page } from '@playwright/test'
import { PASSWORD, USER, cli, deliver, destroyBySubject, htmlMessage, login, textMessage } from './dev'

const nav = (page: Page, mailbox: string) => page.locator(`a[data-mailbox="${mailbox}"]`)
const row = (page: Page, subject: string) => page.locator('[data-row]', { hasText: subject })

// The number a mailbox shows in the navigation (unread, or the total of
// Trash); 0 when it shows none.
async function count(page: Page, mailbox: string, kind: 'unread' | 'total'): Promise<number> {
  const el = nav(page, mailbox).locator(`[data-${kind}]`)
  return (await el.count()) ? Number(await el.innerText()) : 0
}

test('read, flag, move and delete a message', async ({ page, baseURL }) => {
  const problems: string[] = []
  page.on('pageerror', (e) => problems.push(`pageerror: ${e.message}`))
  page.on('console', (m) => {
    if (m.type() !== 'error') return
    const text = m.text()
    // Expected: the 401 of /auth/session before the login (how the app
    // learns that nobody is logged in); the remote image of the test
    // message, whose host does not exist; and Playwright's own scripts,
    // which the sandbox of the message frame refuses like any other.
    if (/401|ERR_NAME_NOT_RESOLVED|Blocked script execution in '[^']*\/jmap\/view\//.test(text)) return
    problems.push(`console: ${text}`)
  })
  // The page talks to its own origin only. The one exception is the
  // remote image, after the reader asked for it.
  const foreign: string[] = []
  page.on('request', (r) => {
    const u = new URL(r.url())
    if (u.protocol.startsWith('http') && u.origin !== new URL(baseURL!).origin) foreign.push(r.url())
  })

  const stamp = Date.now()
  const subject = `E2E digest ${stamp}`
  const boxName = `E2E box ${stamp}`
  deliver(htmlMessage(subject))

  // Log in. Nothing but the cookie holds the session.
  await login(page)
  await expect(nav(page, 'inbox')).toHaveAttribute('aria-current', 'page')
  await expect(row(page, subject)).toBeVisible()
  await expect(row(page, subject)).toHaveAttribute('data-unread', 'true')
  const unread = await count(page, 'inbox', 'unread')
  expect(unread).toBeGreaterThan(0)
  const trash = await count(page, 'trash', 'total')
  await expect(page).toHaveTitle(new RegExp(`^\\(${unread}\\) Inbox`))

  // Open it: the HTML body is the sanitised view in a sandboxed iframe.
  await row(page, subject).getByRole('link').click()
  await expect(page).toHaveURL(/\/mail\/m\d+\/t\w+$/)
  await expect(page.getByTestId('thread-subject')).toHaveText(subject)
  const frame = page.getByTestId('html-view')
  await expect(frame).toHaveAttribute('src', /^\/jmap\/view\/[^/]+\/e\w+$/)
  await expect(frame).toHaveAttribute('sandbox', 'allow-popups allow-popups-to-escape-sandbox')
  const body = page.frameLocator('[data-testid="html-view"]')
  await expect(body.getByRole('heading', { name: 'Five things worth reading' })).toBeVisible()
  await expect(body.locator('script')).toHaveCount(0)
  const viewed = await page.request.get((await frame.getAttribute('src'))!)
  expect(viewed.headers()['content-security-policy']).toContain("default-src 'none'")
  expect(await viewed.text()).not.toMatch(/<script|document\.title/)
  expect(foreign).toEqual([])
  await expect(body.locator('img')).not.toHaveAttribute('src', /lists\.example/)

  // The server said it left remote images out; ask for them.
  await page.getByTestId('load-images').click()
  await expect(page.getByTestId('html-view')).toHaveAttribute('src', /\?images=1$/)
  await expect(page.frameLocator('[data-testid="html-view"]').locator('img')).toHaveAttribute('src', 'https://lists.example/header.png')
  await expect(page.getByTestId('load-images')).toHaveCount(0)

  // The attachment downloads with its content.
  const attachment = page.getByTestId('attachment')
  await expect(attachment).toContainText('notes.txt')
  const file = await page.request.get((await attachment.getAttribute('href'))!)
  expect(file.status()).toBe(200)
  expect(file.headers()['content-disposition']).toContain('attachment')
  expect((await file.text()).trim()).toBe('These are the notes.')

  // Reading marked it read: the row and the count follow.
  await expect(row(page, subject)).not.toHaveAttribute('data-unread', 'true')
  await expect.poll(() => count(page, 'inbox', 'unread')).toBe(unread - 1)

  // Flag it, in the thread and so in the list.
  const flagged = page.waitForResponse((r) => r.url().endsWith('/jmap/api') && (r.request().postData() ?? '').includes('keywords/$flagged'))
  await page.getByTestId('thread-flag').click()
  await expect(page.getByTestId('thread-flag')).toHaveAttribute('aria-pressed', 'true')
  expect((await flagged).status()).toBe(200)
  await expect(row(page, subject).getByTestId('row-flag')).toHaveAttribute('aria-pressed', 'true')

  // A reload brings back the same mailbox and thread, from the server:
  // read, flagged.
  await page.reload()
  await expect(page.getByTestId('thread-subject')).toHaveText(subject)
  await expect(page.getByTestId('thread-flag')).toHaveAttribute('aria-pressed', 'true')
  await expect(row(page, subject)).not.toHaveAttribute('data-unread', 'true')
  await expect.poll(() => count(page, 'inbox', 'unread')).toBe(unread - 1)

  // The filter is part of the address: flagged mail only.
  await page.getByTestId('filter').click()
  await page.getByRole('checkbox', { name: 'Flagged' }).click()
  await expect(page).toHaveURL(/[?&]flagged=1/)
  await expect(row(page, subject)).toBeVisible()
  await expect(page.locator('[data-row] [data-testid="row-flag"][aria-pressed="false"]')).toHaveCount(0)
  await page.getByRole('button', { name: 'Done' }).click()
  await page.getByRole('button', { name: 'Remove filter Flagged' }).click()
  await expect(page).not.toHaveURL(/flagged/)

  // A new mailbox, and the message moved into it.
  await page.getByRole('button', { name: 'New mailbox' }).click()
  await page.getByRole('dialog').getByLabel('Name').fill(boxName)
  await page.getByRole('dialog').getByRole('button', { name: 'Create' }).click()
  await expect(nav(page, boxName)).toHaveAttribute('aria-current', 'page')
  await expect(page.getByText('No messages here.')).toBeVisible()

  await nav(page, 'inbox').click()
  await row(page, subject).getByRole('link').click()
  await page.getByTestId('thread-move').click()
  await page.getByRole('menuitem', { name: boxName }).click()
  await expect(row(page, subject)).toHaveCount(0)
  await expect.poll(() => count(page, 'inbox', 'unread')).toBe(unread - 1)

  await nav(page, boxName).click()
  await expect(row(page, subject)).toBeVisible()
  await expect(row(page, subject).getByTestId('row-flag')).toHaveAttribute('aria-pressed', 'true')

  // Delete: to Trash, whose count goes up.
  await row(page, subject).getByRole('link').click()
  await expect(page.getByTestId('thread-subject')).toHaveText(subject)
  await page.getByTestId('thread-delete').click()
  await expect(row(page, subject)).toHaveCount(0)
  await expect(page.getByText('No messages here.')).toBeVisible()
  await expect.poll(() => count(page, 'trash', 'total')).toBe(trash + 1)

  // Delete from Trash: gone for good, and the count is back.
  await nav(page, 'trash').click()
  await row(page, subject).getByRole('link').click()
  await expect(page.getByTestId('thread-subject')).toHaveText(subject)
  await page.getByRole('button', { name: 'Delete forever' }).click()
  await expect(row(page, subject)).toHaveCount(0)
  await expect.poll(() => count(page, 'trash', 'total')).toBe(trash)

  // The server agrees: the same session, asked in plain JMAP.
  const session = await (await page.request.get('/.well-known/jmap')).json()
  const accountId = session.primaryAccounts['urn:ietf:params:jmap:mail']
  const api = await page.request.post('/jmap/api', {
    data: {
      using: ['urn:ietf:params:jmap:core', 'urn:ietf:params:jmap:mail'],
      methodCalls: [['Email/query', { accountId, filter: { subject } }, 'q']],
    },
  })
  expect((await api.json()).methodResponses[0][1].ids).toEqual([])

  // Remove the mailbox again, while it is the open one: the view falls
  // back to the inbox.
  await nav(page, boxName).click()
  await expect(nav(page, boxName)).toHaveAttribute('aria-current', 'page')
  await nav(page, boxName).hover()
  await page.getByRole('button', { name: `Actions for ${boxName}` }).click()
  await page.getByRole('menuitem', { name: 'Delete' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Delete' }).click()
  await expect(nav(page, boxName)).toHaveCount(0)
  await expect(nav(page, 'inbox')).toHaveAttribute('aria-current', 'page')
  await expect(page.getByRole('dialog')).toHaveCount(0)

  // Keyboard: j opens the first conversation, Escape returns to the list,
  // and "back" works on what the keys did.
  await expect(page.locator('[data-row]').first()).toBeVisible()
  const firstThread = await page.locator('[data-row]').first().getAttribute('data-thread')
  const first = page.locator(`[data-thread="${firstThread}"]`)
  const wasUnread = (await first.getAttribute('data-unread')) === 'true'
  await page.keyboard.press('j')
  await expect(page).toHaveURL(new RegExp(`/mail/m\\d+/${firstThread}$`))
  await expect(page.getByTestId('thread-subject')).toBeVisible()
  await expect(first).not.toHaveAttribute('data-unread', 'true')
  await page.keyboard.press('Escape')
  await expect(page).toHaveURL(/\/mail\/m\d+$/)
  await page.goBack() // "j" pushed the conversation; Escape replaced it by the list
  await expect(page).toHaveURL(/\/mail\/m\d+$/)
  // Opening the conversation marked it read; put that back, with the
  // selection and its bar.
  if (wasUnread) {
    await first.getByRole('checkbox').click()
    await expect(page.getByTestId('selected-count')).toHaveText('1 selected')
    await page.getByRole('button', { name: 'Mark unread' }).click()
    await expect(first).toHaveAttribute('data-unread', 'true')
    await expect(page.getByTestId('selected-count')).toHaveCount(0)
  }
  await expect.poll(() => count(page, 'inbox', 'unread')).toBe(unread - 1)

  // Nothing secret is kept by the page: the session is an HttpOnly cookie.
  const stored = await page.evaluate(() => JSON.stringify({ ...localStorage }) + JSON.stringify({ ...sessionStorage }) + document.cookie)
  expect(stored).not.toContain(PASSWORD)
  expect(stored).not.toMatch(/token|robinmail=/i)

  // Log out. The app pages need a login again, and so does the API.
  await page.getByTestId('logout').click()
  await expect(page).toHaveURL(/\/login$/)
  await page.goto('/mail/m1')
  await expect(page).toHaveURL(/\/login\?next=/)
  expect((await page.request.post('/jmap/api', { data: { using: [], methodCalls: [] } })).status()).toBe(401)

  expect(problems).toEqual([])
  expect(foreign).toEqual(['https://lists.example/header.png'])
})

test('a session that ended returns to the login, and then to where it was', async ({ page, context }) => {
  await login(page)
  await expect(page.locator('[data-row]').first()).toBeVisible()
  // The server forgets the session (as after 30 days, or a logout elsewhere).
  // JSON, as everything under /auth that changes something has to be.
  expect((await context.request.post('/auth/logout', { data: {} })).status()).toBe(204)
  await page.getByRole('button', { name: 'Check for new mail' }).click()
  await expect(page).toHaveURL(/\/login\?next=\/mail\/m\d+$/)
  await page.getByLabel('Email address').fill('dev@robinmail.test')
  await page.getByLabel('Password').fill(PASSWORD)
  await page.getByRole('button', { name: 'Log in' }).click()
  await expect(page).toHaveURL(/\/mail\/m\d+$/)
  await expect(page.locator('[data-row]').first()).toBeVisible()
})

test('a wrong password says so', async ({ page }) => {
  await page.goto('/login')
  await page.getByLabel('Email address').fill('dev@robinmail.test')
  await page.getByLabel('Password').fill('not the password')
  await page.getByRole('button', { name: 'Log in' }).click()
  await expect(page.getByRole('alert')).toContainText('Wrong address or password.')
  await expect(page).toHaveURL(/\/login$/)
})

test('phone width: the list, then the conversation, then back', async ({ browser }) => {
  const context = await browser.newContext({ viewport: { width: 390, height: 844 }, hasTouch: true })
  const page = await context.newPage()
  await login(page)
  const list = page.getByTestId('message-list')
  await expect(list).toBeVisible()
  await expect(nav(page, 'inbox')).toBeHidden() // the mailboxes are a drawer
  const first = page.locator('[data-row]', { hasText: 'Lunch' }).first()
  await first.getByRole('link').click()
  await expect(page.getByTestId('thread')).toBeVisible()
  await expect(list).toBeHidden()
  // Nothing is wider than the screen.
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(390)
  await page.goBack()
  await expect(list).toBeVisible()
  await expect(page.getByTestId('thread')).toHaveCount(0)
  // Reading marked the reply read; put that back.
  await destroyBySubject(page, 'no message has this subject', ['Re: Lunch on Tuesday'])
  await context.close()
})

test('a long list loads page by page, and a mailbox is deleted with its mail', async ({ page }) => {
  const stamp = Date.now()
  const boxName = `E2E paging ${stamp}`
  cli('mailbox', 'create', USER, boxName)
  for (let n = 1; n <= 35; n++) deliver(textMessage(`Paging ${stamp} ${String(n).padStart(2, '0')}`, n), boxName)

  await login(page)
  await expect(nav(page, boxName).locator('[data-unread]')).toHaveText('35')
  await nav(page, boxName).click()
  const rows = page.locator('[data-row]')
  await expect(rows).toHaveCount(30) // one page
  await expect(rows.first()).toContainText(`Paging ${stamp} 35`) // newest first: the one delivered last
  await expect(page.getByText('35 conversations')).toBeVisible()

  // The end of the list coming into view loads the rest.
  await rows.last().scrollIntoViewIfNeeded()
  await expect(rows).toHaveCount(35)
  await expect(rows.last()).toContainText(`Paging ${stamp} 01`)
  await expect(page.getByText('End of the list')).toBeVisible()

  // Several at once: select three, mark them read, the count follows.
  for (const i of [0, 1, 2]) await rows.nth(i).getByRole('checkbox').click()
  await expect(page.getByTestId('selected-count')).toHaveText('3 selected')
  await page.getByRole('button', { name: 'Mark read' }).click()
  await expect(nav(page, boxName).locator('[data-unread]')).toHaveText('32')
  await expect(rows.nth(0)).not.toHaveAttribute('data-unread', 'true')
  await expect(rows.nth(3)).toHaveAttribute('data-unread', 'true')

  // And delete two of them: to Trash.
  const trash = await count(page, 'trash', 'total')
  for (const i of [0, 1]) await rows.nth(i).getByRole('checkbox').click()
  await page.getByRole('button', { name: 'Delete', exact: true }).click()
  await expect(rows).toHaveCount(33)
  await expect.poll(() => count(page, 'trash', 'total')).toBe(trash + 2)
  await expect(page.getByText('33 conversations')).toBeVisible()

  // Deleting the mailbox says what happens to its mail, and does it.
  await nav(page, boxName).hover()
  await page.getByRole('button', { name: `Actions for ${boxName}` }).click()
  await page.getByRole('menuitem', { name: 'Delete' }).click()
  await expect(page.getByRole('dialog')).toContainText('Its 33 messages are deleted with it.')
  await page.getByRole('dialog').getByRole('button', { name: 'Delete' }).click()
  await expect(nav(page, boxName)).toHaveCount(0)
  await expect(nav(page, 'inbox')).toHaveAttribute('aria-current', 'page')

  // Empty the two from Trash again.
  await nav(page, 'trash').click()
  const mine = page.locator('[data-row]', { hasText: `Paging ${stamp}` })
  await expect(mine).toHaveCount(2)
  for (const i of [0, 1]) await mine.nth(i).getByRole('checkbox').click()
  await page.getByRole('button', { name: 'Delete forever' }).click()
  await expect(mine).toHaveCount(0)
  await expect.poll(() => count(page, 'trash', 'total')).toBe(trash)

  // The page shows a change before the server has it. Ask the server.
  const session = await (await page.request.get('/.well-known/jmap')).json()
  const accountId = session.primaryAccounts['urn:ietf:params:jmap:mail']
  await expect.poll(async () => {
    const r = await page.request.post('/jmap/api', {
      data: {
        using: ['urn:ietf:params:jmap:core', 'urn:ietf:params:jmap:mail'],
        methodCalls: [['Email/query', { accountId, filter: { subject: `Paging ${stamp}` } }, 'q']],
      },
    })
    return (await r.json()).methodResponses[0][1].ids
  }).toEqual([])
})
