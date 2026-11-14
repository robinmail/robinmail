// The browser half of phase 5: write a message with an attachment, send
// it to another user of the same server, see it arrive, answer it, and
// see what the server says became of it. It runs against a real robinmail
// (see playwright.config.ts) with the two users of contrib/dev/seed.sh.
// Nothing leaves the machine: robinmail.test is delivered inside the
// server and every other domain is routed to Mailpit, a mail catcher in
// Docker that passes nothing on (contrib/compose/compose.yaml).
//
// A send takes a while on purpose: the server holds a message for
// undo_seconds (10) and its queue looks every 5 seconds.
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { expect, test, type Browser, type Locator, type Page } from '@playwright/test'
import { OTHER, PASSWORD, USER, cleanUp, deliver, emailsBySubject, jmap, login } from './dev'

test.describe.configure({ timeout: 120_000 })

const nav = (page: Page, mailbox: string) => page.locator(`a[data-mailbox="${mailbox}"]`)
const row = (page: Page, subject: string) => page.locator('[data-row]', { hasText: subject })
const composer = (page: Page) => page.getByTestId('composer').last()
const to = (c: Locator) => c.getByLabel('To', { exact: true })
const subjectOf = (c: Locator) => c.getByLabel('Subject')
const body = (c: Locator) => c.getByTestId('plain-body')
const DELIVERY = 40_000

// problems collects what must not happen on a page: errors, and requests
// to anywhere but the server itself.
function watch(page: Page, baseURL: string): string[] {
  const problems: string[] = []
  page.on('pageerror', (e) => problems.push(`pageerror: ${e.message}`))
  page.on('console', (m) => {
    if (m.type() !== 'error') return
    // The 401 of /auth/session before a login, and Playwright's own
    // scripts, which the sandbox of the message frame refuses.
    if (/401|Blocked script execution in '[^']*\/jmap\/view\//.test(m.text())) return
    problems.push(`console: ${m.text()}`)
  })
  page.on('request', (r) => {
    const u = new URL(r.url())
    if (u.protocol.startsWith('http') && u.origin !== new URL(baseURL).origin) problems.push(`foreign request: ${r.url()}`)
  })
  return problems
}

async function open(browser: Browser, user: string, baseURL: string): Promise<{ page: Page; problems: string[] }> {
  const context = await browser.newContext()
  const page = await context.newPage()
  const problems = watch(page, baseURL)
  await login(page, user)
  await expect(page.getByTestId('compose')).toBeEnabled()
  return { page, problems }
}

// arrived waits for a row with the subject, asking the server now and then.
async function arrived(page: Page, subject: string): Promise<Locator> {
  await expect(async () => {
    await page.getByRole('button', { name: 'Check for new mail' }).click()
    await expect(row(page, subject)).toBeVisible({ timeout: 2000 })
  }).toPass({ timeout: DELIVERY })
  return row(page, subject)
}

test('send with an attachment, receive, reply: the thread and $answered', async ({ browser, baseURL }) => {
  const stamp = Date.now()
  const subject = `E2E send ${stamp}`
  const a = await open(browser, USER, baseURL!)
  const b = await open(browser, OTHER, baseURL!)

  // Write it. "c" opens the composer.
  await a.page.keyboard.press('c')
  const c = composer(a.page)
  await expect(to(c)).toBeFocused()
  await to(c).fill(`Other <${OTHER}>`)
  await to(c).press('Enter')
  await expect(c.getByTestId('address-chip')).toHaveText(['Other'])
  await subjectOf(c).fill(subject)
  await body(c).fill('Hello,\n\nThe notes are attached.\n')
  await c.getByTestId('composer-file').setInputFiles({ name: 'notes.txt', mimeType: 'text/plain', buffer: Buffer.from('These are the notes of the test.\n') })
  await expect(c.getByTestId('composer-attachment')).toHaveAttribute('data-state', 'done')
  await expect(c.getByTestId('composer-attachment')).toContainText('notes.txt')
  await expect(c.getByTestId('composer-status')).toHaveText('Saved')

  // Send with the keyboard. The composer goes, the notice offers to undo.
  await body(c).press('ControlOrMeta+Enter')
  await expect(a.page.getByTestId('composer')).toHaveCount(0)
  await expect(a.page.getByText('Message sent', { exact: true })).toBeVisible()

  // It is in Sent, not in Drafts, without $draft.
  await nav(a.page, 'sent').click()
  await expect(row(a.page, subject)).toBeVisible()
  const mine = await emailsBySubject(a.page, subject)
  expect(mine).toHaveLength(1)
  expect(mine[0]!.keywords).toEqual({ $seen: true })
  await row(a.page, subject).getByRole('link').click()
  const status = a.page.getByTestId('delivery').locator('li')
  await expect(status).toHaveCount(1)
  await expect(status).toContainText(OTHER)

  // The other user gets it, with the attachment.
  await (await arrived(b.page, subject)).getByRole('link').click()
  await expect(b.page.getByTestId('thread-subject')).toHaveText(subject)
  await expect(b.page.getByTestId('text-view')).toContainText('The notes are attached.')
  const attachment = b.page.getByTestId('attachment')
  await expect(attachment).toContainText('notes.txt')
  const file = await b.page.request.get((await attachment.getAttribute('href'))!)
  expect(await file.text()).toBe('These are the notes of the test.\n')

  // By now the sender sees it as delivered: a mailbox on this server.
  await expect(status).toHaveAttribute('data-delivery', 'Delivered', { timeout: DELIVERY })

  // The other user answers ("r").
  await b.page.keyboard.press('r')
  const reply = composer(b.page)
  await expect(reply.getByTestId('address-chip')).toHaveText([USER])
  await expect(subjectOf(reply)).toHaveValue(`Re: ${subject}`)
  await expect(body(reply)).toBeFocused()
  await expect(body(reply)).toHaveValue(/wrote:\n> Hello,\n>\n> The notes are attached\.$/)
  await b.page.keyboard.type('Thank you, got them.')
  await reply.getByTestId('composer-send').click()
  await expect(b.page.getByTestId('composer')).toHaveCount(0)

  // The original is marked answered, and the thread has both messages.
  await expect(b.page.locator('article[data-email]')).toHaveCount(2)
  await expect(b.page.locator('article[data-email]').first().getByTestId('answered')).toBeVisible()
  const theirs = await emailsBySubject(b.page, subject)
  expect(theirs.map((e) => !!e.keywords.$answered).sort()).toEqual([false, true])
  expect(new Set(theirs.map((e) => e.threadId)).size).toBe(1)

  // The answer arrives in the thread of the message that was sent.
  await nav(a.page, 'inbox').click()
  await (await arrived(a.page, `Re: ${subject}`)).getByRole('link').click()
  await expect(a.page.locator('article[data-email]')).toHaveCount(2)
  await expect(a.page.getByTestId('text-view').last()).toContainText('Thank you, got them.')
  const thread = await emailsBySubject(a.page, subject, ['threadId', 'inReplyTo', 'references', 'messageId'])
  expect(new Set(thread.map((e) => e.threadId)).size).toBe(1)
  const [first, answer] = thread.sort((x, y) => (x.inReplyTo ? 1 : 0) - (y.inReplyTo ? 1 : 0))
  expect(answer!.inReplyTo).toEqual(first!.messageId)
  expect(answer!.references).toEqual(first!.messageId)

  expect(a.problems).toEqual([])
  expect(b.problems).toEqual([])
  await cleanUp(a.page, subject)
  await cleanUp(b.page, subject)
})

test('a draft survives a reload, opens from Drafts, and is discarded', async ({ browser, baseURL }) => {
  const subject = `E2E draft ${Date.now()}`
  const { page, problems } = await open(browser, USER, baseURL!)

  await page.getByTestId('compose').click()
  const c = composer(page)
  await to(c).fill('ann@example.org, bob@example.org')
  await subjectOf(c).fill(subject)
  await body(c).fill('First version')

  // Leaving before it is saved asks first.
  let asked = ''
  page.once('dialog', (d) => {
    asked = d.type()
    void d.dismiss()
  })
  await page.evaluate(() => location.reload())
  await expect.poll(() => asked).toBe('beforeunload')
  await expect(body(c)).toHaveValue('First version')

  await expect(c.getByTestId('composer-status')).toHaveText('Saved')
  await page.reload()
  await expect(page.getByTestId('composer')).toHaveCount(0)

  // It is in Drafts and opens in the composer again.
  await nav(page, 'drafts').click()
  await row(page, subject).getByRole('link').click()
  await expect(subjectOf(c)).toHaveValue(subject)
  await expect(body(c)).toHaveValue('First version')
  await expect(c.getByTestId('address-chip')).toHaveText(['ann@example.org', 'bob@example.org'])
  await expect(page).toHaveURL(/\/mail\/m\d+$/) // not opened as a thread

  // A change replaces the draft: there is still one.
  await body(c).fill('Second version')
  await expect(c.getByTestId('composer-status')).toHaveText('Saved')
  const saved = await emailsBySubject(page, subject)
  expect(saved).toHaveLength(1)
  expect(saved[0]!.keywords).toEqual({ $draft: true, $seen: true })
  await expect(row(page, subject)).toHaveCount(1)
  await expect(row(page, subject)).toContainText('Second version')

  // Close keeps it, discard removes it.
  await c.getByTestId('composer-close').click()
  await expect(page.getByTestId('composer')).toHaveCount(0)
  await row(page, subject).getByRole('link').click()
  await expect(body(c)).toHaveValue('Second version')
  await c.getByTestId('composer-discard').click()
  await c.getByTestId('composer-discard').click()
  await expect(page.getByTestId('composer')).toHaveCount(0)
  await expect(row(page, subject)).toHaveCount(0)
  expect(await emailsBySubject(page, subject)).toHaveLength(0)
  expect(problems).toEqual([])
})

test('undo a send: the message is a draft again and never arrives', async ({ browser, baseURL }) => {
  const subject = `E2E undo ${Date.now()}`
  const a = await open(browser, USER, baseURL!)
  const b = await open(browser, OTHER, baseURL!)

  await a.page.getByTestId('compose').click()
  const c = composer(a.page)
  await to(c).fill(OTHER)
  await subjectOf(c).fill(subject)
  await body(c).fill('I should not have sent this.')
  await c.getByTestId('composer-send').click()
  await expect(a.page.getByTestId('composer')).toHaveCount(0)
  await a.page.getByRole('button', { name: 'Undo' }).click()

  // The composer is back with the text, and the message is in Drafts.
  await expect(body(c)).toHaveValue('I should not have sent this.')
  await expect(c.getByTestId('address-chip')).toHaveText([OTHER])
  await expect(c.getByTestId('composer-status')).toHaveText('Saved')
  const mine = await emailsBySubject(a.page, subject)
  expect(mine).toHaveLength(1)
  expect(mine[0]!.keywords.$draft).toBe(true)
  const [subs] = await jmap(a.page, [['EmailSubmission/get', { ids: null }]])
  expect(subs!.list.filter((s: { undoStatus: string }) => s.undoStatus === 'canceled')).toHaveLength(1)

  // Well past the time the server would have sent it, nothing arrived.
  await a.page.waitForTimeout(17_000)
  expect(await emailsBySubject(b.page, subject)).toHaveLength(0)

  await c.getByTestId('composer-discard').click()
  await c.getByTestId('composer-discard').click()
  expect(a.problems).toEqual([])
  await cleanUp(a.page, subject)
})

test('mail to an unknown local user: failed delivery status and the bounce', async ({ browser, baseURL }) => {
  const stamp = Date.now()
  const subject = `E2E bounce ${stamp}`
  const nobody = `nobody-${stamp}@robinmail.test`
  const { page, problems } = await open(browser, USER, baseURL!)

  await page.getByTestId('compose').click()
  const c = composer(page)
  await to(c).fill(`${OTHER}, ${nobody}`)
  await subjectOf(c).fill(subject)
  await body(c).fill('One of you does not exist.')
  await c.getByTestId('composer-send').click()
  await expect(page.getByTestId('composer')).toHaveCount(0)

  await nav(page, 'sent').click()
  await row(page, subject).getByRole('link').click()
  const failed = page.getByTestId('delivery').locator('li', { hasText: nobody })
  const fine = page.getByTestId('delivery').locator('li', { hasText: OTHER })
  await expect(failed).toHaveAttribute('data-delivery', 'Sending')
  await expect(failed).toHaveAttribute('data-delivery', 'Failed', { timeout: DELIVERY })
  await expect(failed).toContainText('550 5.1.1')
  await expect(fine).toHaveAttribute('data-delivery', 'Delivered')

  // The bounce is in the Inbox, in the thread of the message.
  await expect(page.locator('article[data-email]')).toHaveCount(2, { timeout: DELIVERY })
  await expect(page.locator('article[data-email]').last()).toContainText('MAILER-DAEMON')
  await expect(page.locator('article[data-email]').last()).toContainText(nobody)
  await nav(page, 'inbox').click()
  await expect(row(page, subject)).toBeVisible()
  await expect(row(page, subject)).toHaveAttribute('data-unread', 'true')

  expect(problems).toEqual([])
  await cleanUp(page, subject)
  const b = await open(browser, OTHER, baseURL!)
  await cleanUp(b.page, subject)
})

test('rich text with a pasted image arrives as HTML with the image inline', async ({ browser, baseURL }) => {
  const subject = `E2E rich ${Date.now()}`
  const a = await open(browser, USER, baseURL!)
  const b = await open(browser, OTHER, baseURL!)

  await a.page.getByTestId('compose').click()
  const c = composer(a.page)
  await to(c).fill(OTHER)
  await subjectOf(c).fill(subject)
  await body(c).fill('Plain start')
  await c.getByTestId('composer-rich').click()
  const rich = c.getByTestId('rich-body')
  await expect(rich).toHaveText('Plain start')
  await rich.click()
  await a.page.keyboard.press('ControlOrMeta+a')
  await c.getByRole('button', { name: 'Bold' }).click()
  await a.page.keyboard.press('ArrowRight')
  await a.page.keyboard.press('Enter')
  await c.getByRole('button', { name: 'Bold' }).click()
  await c.getByRole('button', { name: 'Bulleted list' }).click()
  await a.page.keyboard.type('first item')
  await a.page.keyboard.press('Enter')
  await a.page.keyboard.type('second item')
  // Pasted markup comes in as text.
  await rich.evaluate((el) => {
    const data = new DataTransfer()
    data.setData('text/html', '<img src="https://tracker.example/x.png"><script>window.pwned = 1</script><b>bold paste</b>')
    data.setData('text/plain', ' pasted <b>text</b>')
    el.dispatchEvent(new ClipboardEvent('paste', { clipboardData: data, bubbles: true, cancelable: true }))
  })
  await expect(rich).toContainText('second item pasted <b>text</b>')
  expect(await rich.locator('img').count()).toBe(0)
  // A pasted image is uploaded and shown in the text.
  await rich.evaluate((el) => {
    const png = Uint8Array.from(atob('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=='), (ch) => ch.charCodeAt(0))
    const data = new DataTransfer()
    data.items.add(new File([png], 'pixel.png', { type: 'image/png' }))
    el.dispatchEvent(new ClipboardEvent('paste', { clipboardData: data, bubbles: true, cancelable: true }))
  })
  await expect(rich.locator('img')).toHaveAttribute('src', /^\/jmap\/download\//)
  await expect(c.getByTestId('composer-attachment')).toContainText('in the text')
  await expect(c.getByTestId('composer-status')).toHaveText('Saved')

  // The stored draft is text plus HTML, the image inline by Content-ID.
  const [draft] = await emailsBySubject(a.page, subject, ['id', 'textBody', 'htmlBody', 'attachments', 'bodyValues'])
  expect(draft!.htmlBody[0].type).toBe('text/html')
  expect(draft!.textBody[0].type).toBe('text/plain')
  expect(draft!.attachments).toHaveLength(1)
  expect(draft!.attachments[0]).toMatchObject({ type: 'image/png', disposition: 'inline' })

  // Closed and opened again it is the same rich message.
  await c.getByTestId('composer-close').click()
  await nav(a.page, 'drafts').click()
  await row(a.page, subject).getByRole('link').click()
  await expect(rich.locator('b').first()).toHaveText('Plain start')
  await expect(rich.locator('li')).toHaveCount(2)
  await expect(rich.locator('img')).toHaveAttribute('src', /^\/jmap\/download\//)
  expect(await a.page.evaluate(() => (window as unknown as { pwned?: number }).pwned)).toBeUndefined()

  await c.getByTestId('composer-send').click()
  await expect(a.page.getByTestId('composer')).toHaveCount(0)

  await (await arrived(b.page, subject)).getByRole('link').click()
  const view = b.page.frameLocator('[data-testid="html-view"]')
  await expect(view.locator('b').first()).toHaveText('Plain start')
  await expect(view.locator('li')).toHaveText(['first item', 'second item pasted <b>text</b>'])
  await expect(view.locator('img')).toHaveAttribute('src', /^data:image\/png;base64,/)
  const [, text] = await jmap(b.page, [
    ['Email/query', { filter: { subject } }],
    ['Email/get', { '#ids': { resultOf: 'c0', name: 'Email/query', path: '/ids' }, properties: ['textBody', 'bodyValues'], fetchTextBodyValues: true }],
  ])
  expect(Object.values(text!.list[0].bodyValues as Record<string, { value: string }>)[0]!.value).toContain('- first item\n- second item pasted <b>text</b>')

  expect(a.problems).toEqual([])
  expect(b.problems).toEqual([])
  await cleanUp(a.page, subject)
  await cleanUp(b.page, subject)
})

test('reply all, forward with the attachment, mailto: links and an identity with a signature', async ({ browser, baseURL }) => {
  const stamp = Date.now()
  const subject = `E2E links ${stamp}`
  deliver(`From: Ann <ann@example.org>
To: ${USER}, Bob <bob@example.org>
Cc: carol@example.org
Subject: ${subject}
Date: ${new Date().toUTCString().replace('GMT', '+0000')}
Message-ID: <links${stamp}@example.org>
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="mix"

--mix
Content-Type: text/plain; charset=utf-8

Write to desk@example.org or see https://example.org/help.
--mix
Content-Type: text/plain; name="list.txt"
Content-Disposition: attachment; filename="list.txt"

a list
--mix--
`)
  const { page, problems } = await open(browser, USER, baseURL!)

  // An identity with a tag and a signature.
  await page.getByRole('link', { name: 'Identities and signatures' }).click()
  await page.getByTestId('add-identity').click()
  await page.getByLabel('Address').fill(`t${stamp}`)
  await page.getByLabel('Name').fill('Dev at the shop')
  await page.getByLabel('Signature').fill('The Shop\nOpen daily')
  await page.getByRole('button', { name: 'Add', exact: true }).click()
  const tagged = `dev+t${stamp}@robinmail.test`
  await expect(page.getByTestId('identity').filter({ hasText: tagged })).toContainText('The Shop')
  await page.getByRole('link', { name: 'Back to the mail' }).click()

  await row(page, subject).getByRole('link').click()
  const text = page.getByTestId('text-view')

  // A mail address in the text opens the composer.
  await text.getByRole('link', { name: 'desk@example.org' }).click()
  let c = composer(page)
  await expect(c.getByTestId('address-chip')).toHaveText(['desk@example.org'])
  await expect(text.getByRole('link', { name: 'https://example.org/help' })).toHaveAttribute('target', '_blank')
  // The other identity brings its signature.
  await c.getByLabel('From').click()
  await page.getByRole('option', { name: new RegExp(`t${stamp}`) }).click()
  await expect(body(c)).toHaveValue('\n\n-- \nThe Shop\nOpen daily')
  await c.getByTestId('composer-discard').click()
  await c.getByTestId('composer-discard').click() // it was changed, so it asks
  await expect(page.getByTestId('composer')).toHaveCount(0)

  // Reply all ("a"): the sender, and the others without ourselves.
  await page.keyboard.press('a')
  c = composer(page)
  await expect(c.getByTestId('address-chip')).toHaveText(['Ann', 'Bob', 'carol@example.org'])
  await expect(subjectOf(c)).toHaveValue(`Re: ${subject}`)
  await c.getByTestId('composer-discard').click()

  // Forward ("f"): the text and the attachment of the original.
  await page.keyboard.press('f')
  c = composer(page)
  await expect(subjectOf(c)).toHaveValue(`Fwd: ${subject}`)
  await expect(body(c)).toHaveValue(/---------- Forwarded message ----------\nFrom: Ann <ann@example\.org>/)
  await expect(c.getByTestId('composer-attachment')).toContainText('list.txt')
  await to(c).fill(OTHER)
  await c.getByTestId('composer-send').click()
  await expect(page.getByTestId('composer')).toHaveCount(0)
  // Forward as attachment: the whole message as a file.
  await page.locator('article[data-email]').first().getByRole('button', { name: 'Message actions' }).click()
  await page.getByRole('menuitem', { name: 'Forward as attachment' }).click()
  c = composer(page)
  await expect(c.getByTestId('composer-attachment')).toContainText(`${subject}.eml`)
  await expect(subjectOf(c)).toHaveValue(`Fwd: ${subject}`)
  await to(c).fill(OTHER)
  await c.getByTestId('composer-send').click()
  await expect(page.getByTestId('composer')).toHaveCount(0)

  const all = await emailsBySubject(page, subject, ['keywords', 'mailboxIds', 'attachments'])
  expect(all.find((e) => e.keywords.$forwarded)).toBeTruthy()
  const forwarded = all.filter((e) => !e.keywords.$forwarded).map((e) => e.attachments.map((x: { name: string }) => x.name).join())
  expect(forwarded.sort()).toEqual([`${subject}.eml`, 'list.txt'].sort())
  expect(all.flatMap((e) => e.attachments.map((x: { type: string }) => x.type)).sort()).toEqual(['message/rfc822', 'text/plain', 'text/plain'])

  // Clean up: the identity, the mail, here and at the other user.
  await page.getByRole('link', { name: 'Identities and signatures' }).click()
  await page.getByRole('button', { name: `Remove ${tagged}` }).click()
  await page.getByRole('button', { name: 'Remove', exact: true }).click()
  await expect(page.getByTestId('identity').filter({ hasText: tagged })).toHaveCount(0)
  expect(problems).toEqual([])
  await cleanUp(page, subject)
  const b = await open(browser, OTHER, baseURL!)
  await expect.poll(async () => (await emailsBySubject(b.page, `Fwd: ${subject}`)).length, { timeout: DELIVERY }).toBe(2)
  await cleanUp(b.page, subject)
})

test('an expired session keeps what was typed and saves it after the login', async ({ browser, baseURL }) => {
  const subject = `E2E expired ${Date.now()}`
  const { page } = await open(browser, USER, baseURL!)
  await page.getByTestId('compose').click()
  const c = composer(page)
  await subjectOf(c).fill(subject)
  await body(c).fill('Written before the session ended.')
  await expect(c.getByTestId('composer-status')).toHaveText('Saved')

  // The session ends; the next save is refused and the login page shows.
  await page.context().clearCookies()
  await body(c).pressSequentially(' And this after.')
  await expect(page).toHaveURL(/\/login/)
  await page.getByLabel('Email address').fill(USER)
  await page.getByLabel('Password').fill(PASSWORD)
  await page.getByRole('button', { name: 'Log in' }).click()

  // The composer is back as it was, and now it is saved.
  await expect(body(c)).toHaveValue('Written before the session ended. And this after.')
  await expect(c.getByTestId('composer-status')).toHaveText('Saved')
  const saved = await emailsBySubject(page, subject, ['preview'])
  expect(saved.map((e) => e.preview)).toEqual(['Written before the session ended. And this after.'])
  await c.getByTestId('composer-discard').click()
  await c.getByTestId('composer-discard').click()
  await expect.poll(async () => (await emailsBySubject(page, subject)).length).toBe(0)
})

// Mailpit's HTTP API. The test below is skipped when Mailpit does not
// answer or the development instance does not route outside mail to it.
const MAILPIT = process.env.ROBINMAIL_MAILPIT_URL ?? 'http://127.0.0.1:8025'

function routedToMailpit(): boolean {
  try {
    const config = JSON.parse(readFileSync(fileURLToPath(new URL('../../dev/robinmail.json', import.meta.url)), 'utf8'))
    return /:1025$/.test(config.outbound?.routes?.['*'] ?? '')
  } catch {
    return false
  }
}

test('mail to an outside address reaches Mailpit as a well-formed message', async ({ browser, baseURL, request }) => {
  const up = await request.get(`${MAILPIT}/api/v1/info`, { timeout: 2000 }).then((r) => r.ok(), () => false)
  test.skip(!up, `Mailpit does not answer on ${MAILPIT}: docker compose -f contrib/compose/compose.yaml up -d`)
  test.skip(!routedToMailpit(), 'dev/robinmail.json does not route "*" to Mailpit (127.0.0.1:1025)')

  const stamp = Date.now()
  const subject = `E2E naar buiten ${stamp}: één “lang” onderwerp — met tekens die niet in ASCII passen, en lang genoeg om te vouwen`
  const notes = 'line one\nline two, with LF line ends\n\tand a tab\n'
  const { page, problems } = await open(browser, USER, baseURL!)

  await page.getByTestId('compose').click()
  const c = composer(page)
  await to(c).fill('"Zoë Example, PhD" <zoe@outside.example>, plain@elsewhere.example')
  await c.getByRole('button', { name: 'Add Cc and Bcc' }).click()
  await c.getByLabel('Bcc', { exact: true }).fill('hidden@outside.example')
  await subjectOf(c).fill(subject)
  await c.getByTestId('composer-rich').click()
  const rich = c.getByTestId('rich-body')
  await rich.click()
  await c.getByRole('button', { name: 'Bold' }).click()
  await page.keyboard.type('Beste Zoë,')
  await c.getByRole('button', { name: 'Bold' }).click()
  await page.keyboard.press('Enter')
  await page.keyboard.type(`A line that is long enough to need folding or encoding somewhere on its way: ${'woord '.repeat(30)}einde.`)
  await page.keyboard.press('Enter')
  await c.getByRole('button', { name: 'Bulleted list' }).click()
  await page.keyboard.type('first')
  await page.keyboard.press('Enter')
  await page.keyboard.type('second = 2 & <third>')
  await rich.evaluate((el) => {
    const png = Uint8Array.from(atob('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=='), (ch) => ch.charCodeAt(0))
    const data = new DataTransfer()
    data.items.add(new File([png], 'pixel.png', { type: 'image/png' }))
    el.dispatchEvent(new ClipboardEvent('paste', { clipboardData: data, bubbles: true, cancelable: true }))
  })
  await expect(rich.locator('img')).toHaveCount(1)
  await c.getByTestId('composer-file').setInputFiles({ name: 'notities één.txt', mimeType: 'text/plain', buffer: Buffer.from(notes) })
  await expect(c.getByTestId('composer-attachment')).toHaveCount(2)
  await expect(c.locator('[data-testid="composer-attachment"][data-state="done"]')).toHaveCount(2)
  await c.getByTestId('composer-send').click()
  await expect(page.getByTestId('composer')).toHaveCount(0)

  // Mailpit has it twice: the queue makes one SMTP transaction per domain,
  // one for outside.example (two recipients) and one for elsewhere.example.
  let found: { ID: string; MessageID: string; Bcc: unknown[] | null }[] = []
  await expect(async () => {
    const r = await (await request.get(`${MAILPIT}/api/v1/search`, { params: { query: `subject:"E2E naar buiten ${stamp}"` } })).json()
    expect(r.messages).toHaveLength(2)
    found = r.messages
  }).toPass({ timeout: DELIVERY })
  expect(found[0]!.MessageID).toBe(found[1]!.MessageID)
  const id = found.find((x) => x.Bcc?.length)!.ID
  const m = await (await request.get(`${MAILPIT}/api/v1/message/${id}`)).json()
  expect(m.Subject).toBe(subject)
  expect(m.From).toEqual({ Name: '', Address: USER })
  expect(m.To).toEqual([{ Name: 'Zoë Example, PhD', Address: 'zoe@outside.example' }, { Name: '', Address: 'plain@elsewhere.example' }])
  // The hidden recipient got it (the envelope) and is not named in it (the header).
  expect(m.Bcc).toEqual([{ Name: '', Address: 'hidden@outside.example' }])
  expect(m.ReturnPath).toBe(USER)

  const headers: Record<string, string[]> = await (await request.get(`${MAILPIT}/api/v1/message/${id}/headers`)).json()
  expect(Object.keys(headers).map((h) => h.toLowerCase())).not.toContain('x-robinmail-draft')
  expect(headers['Mime-Version']).toEqual(['1.0'])
  expect(headers['Message-Id']![0]).toMatch(/^<[^<>@\s]+@mail\.robinmail\.test>$/)
  expect(Math.abs(Date.parse(headers.Date![0]!) - Date.now())).toBeLessThan(120_000)
  expect(headers['Content-Type']![0]).toMatch(/^multipart\/mixed;/)
  // Our Received line says nothing about the computer the mail was written on.
  const ours = headers.Received!.find((r) => r.includes('(robinmail)'))!
  expect(ours).toMatch(/^by mail\.robinmail\.test \(robinmail\) \(JMAP\)/)
  expect(ours).not.toMatch(/from |127\.0\.0\.1/)

  // Both bodies, the image inline by its Content-ID, the file as it was.
  expect(m.Text).toContain('Beste Zoë,')
  expect(m.Text).toContain(`${'woord '.repeat(30)}einde.`)
  expect(m.Text.replace(/\r\n/g, '\n')).toContain('- first\n- second = 2 & <third>')
  expect(m.HTML).toContain('<b>Beste Zoë,</b>')
  expect(m.HTML).toContain('</p><ul><li>first</li><li>second = 2 &amp; &lt;third&gt;<img src="cid:')
  expect(m.Inline).toHaveLength(1)
  expect(m.Inline[0]).toMatchObject({ ContentType: 'image/png', FileName: 'pixel.png' })
  expect(m.HTML).toContain(`<img src="cid:${m.Inline[0].ContentID}"`)
  expect(m.Attachments).toHaveLength(1)
  expect(m.Attachments[0]).toMatchObject({ FileName: 'notities één.txt', ContentType: 'text/plain', Size: Buffer.byteLength(notes) })
  const file = await request.get(`${MAILPIT}/api/v1/message/${id}/part/${m.Attachments[0].PartID}`)
  expect((await file.body()).toString()).toBe(notes)
  const image = await request.get(`${MAILPIT}/api/v1/message/${id}/part/${m.Inline[0].PartID}`)
  expect((await image.body()).subarray(0, 8).toString('latin1')).toBe('\x89PNG\r\n\x1a\n')

  // On the wire: mixed[alternative[text, related[html, image]], file], CRLF
  // line ends, nothing 8-bit, no line over 998 octets.
  // Mailpit puts three fields of its own on top: Bcc (from the envelope),
  // Return-Path and Received. What robinmail sent starts at our Received.
  const raw = await (await request.get(`${MAILPIT}/api/v1/message/${id}/raw`)).body()
  const all = raw.toString('latin1')
  const wire = all.slice(all.indexOf('Received: by mail.robinmail.test'))
  expect(all.slice(0, all.length - wire.length)).toMatch(/^Bcc: hidden@outside\.example\r?\nReturn-Path: <dev@robinmail\.test>\r?\nReceived: [^\n]*\n(\s[^\n]*\n)*$/)
  // The hidden recipient is in no header we wrote.
  expect(wire).not.toMatch(/^bcc:/im)
  expect(wire).not.toContain('hidden@outside.example')
  expect(raw.every((b) => b < 0x80)).toBe(true)
  expect(wire.replace(/\r\n/g, '')).not.toMatch(/[\r\n]/)
  expect(Math.max(...wire.split('\r\n').map((l) => l.length))).toBeLessThanOrEqual(998)
  const types = [...wire.matchAll(/^Content-Type: ([a-z]+\/[a-z0-9-]+)/gim)].map((x) => x[1])
  expect(types).toEqual(['multipart/mixed', 'multipart/alternative', 'text/plain', 'multipart/related', 'text/html', 'image/png', 'text/plain'])

  // The webmail shows it as sent, with the other server's reply.
  await nav(page, 'sent').click()
  await row(page, `E2E naar buiten ${stamp}`).getByRole('link').click()
  const status = page.getByTestId('delivery').locator('li')
  await expect(status).toHaveCount(3)
  for (const li of await status.all()) {
    await expect(li).toHaveAttribute('data-delivery', 'Sent', { timeout: DELIVERY })
    await expect(li).toContainText('250')
  }

  expect(problems).toEqual([])
  await cleanUp(page, `E2E naar buiten ${stamp}`)
  await request.delete(`${MAILPIT}/api/v1/messages`, { data: { IDs: found.map((x) => x.ID) } })
})

test('on a phone the composer fills the screen', async ({ browser, baseURL }) => {
  const context = await browser.newContext({ viewport: { width: 390, height: 844 }, hasTouch: true })
  const page = await context.newPage()
  const problems = watch(page, baseURL!)
  await login(page)
  await page.getByTestId('compose-small').click()
  const c = composer(page)
  await expect(c).toBeVisible()
  expect(await c.boundingBox()).toEqual({ x: 0, y: 0, width: 390, height: 844 })
  await subjectOf(c).fill('x')
  await c.getByRole('button', { name: 'Minimise', exact: true }).click()
  const box = (await c.boundingBox())!
  expect(box.width).toBe(390)
  expect(box.y + box.height).toBe(844)
  expect(box.height).toBeLessThan(60)
  await c.getByRole('button', { name: 'Open', exact: true }).click()
  await c.getByTestId('composer-discard').click()
  await c.getByTestId('composer-discard').click()
  await expect(page.getByTestId('composer')).toHaveCount(0)
  expect(problems).toEqual([])
})
