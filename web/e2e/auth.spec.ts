// Phase 7 in the browser: the security settings (password, app passwords,
// two-step login, sessions) and the three ways to log in (password,
// password and code, a provider). Each test makes a user of its own, so
// the development user and the other tests are left alone.

import { createHmac, randomBytes } from 'node:crypto'
import { execFileSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { expect, test, type Browser, type Page } from '@playwright/test'
import jsQR from 'jsqr'
import { USER, angular } from './dev'

const root = fileURLToPath(new URL('../..', import.meta.url))
// A password for the users of these tests, which exist only in the
// development instance.
const PASSWORD = 'e2e-auth-password'

function cli(input: string, ...args: string[]): string {
  return execFileSync('dev/robinmail', ['-config', process.env.ROBINMAIL_E2E_CONFIG ?? 'dev/robinmail.json', ...args], { cwd: root, encoding: 'utf8', input })
}

function newUser(): string {
  const user = `auth-${randomBytes(5).toString('hex')}@robinmail.test`
  cli(`${PASSWORD}\n`, 'user', 'add', user)
  return user
}

async function fillLogin(page: Page, user: string, password = PASSWORD): Promise<void> {
  await page.goto('/')
  await expect(page).toHaveURL(/\/login/)
  await page.getByLabel('Email address').fill(user)
  await page.getByLabel('Password').fill(password)
  await page.getByRole('button', { name: 'Log in', exact: true }).click()
}

async function logIn(page: Page, user: string, password = PASSWORD): Promise<void> {
  await fillLogin(page, user, password)
  await expect(page).toHaveURL(/\/mail\/m\d+$/)
}

async function openSecurity(page: Page): Promise<void> {
  await page.getByRole('link', { name: 'Password and security' }).click()
  await expect(page).toHaveURL(/\/settings\/security$/)
  await expect(page.getByTestId('section-sessions')).toBeVisible()
}

// basicAuth asks JMAP for its session the way a mail program would.
async function basicAuth(baseURL: string, user: string, secret: string): Promise<number> {
  const res = await fetch(`${baseURL}/jmap/session`, { headers: { Authorization: `Basic ${Buffer.from(`${user}:${secret}`).toString('base64')}` } })
  return res.status
}

// RFC 6238 with what authenticator apps use: HMAC-SHA-1, six digits, a
// step of 30 seconds. Written out here; the server's is in internal/totp.
function base32Decode(s: string): Buffer {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'
  let bits = ''
  for (const c of s.replace(/=+$/, '').toUpperCase()) bits += alphabet.indexOf(c).toString(2).padStart(5, '0')
  const bytes = bits.match(/.{8}/g) ?? []
  return Buffer.from(bytes.map((b) => parseInt(b, 2)))
}

function totpCode(secret: string, step: number): string {
  const counter = Buffer.alloc(8)
  counter.writeBigUInt64BE(BigInt(step))
  const mac = createHmac('sha1', base32Decode(secret)).update(counter).digest()
  const offset = mac[mac.length - 1]! & 0x0f
  return String((mac.readUInt32BE(offset) & 0x7fffffff) % 1_000_000).padStart(6, '0')
}

// An authenticator: gives each code once, and never an older one than the
// last, as the server accepts them. The server takes the current step and
// one to either side, so it only waits when the test ran ahead of the clock.
class Authenticator {
  private last = 0
  constructor(private secret: string) {}
  async code(page: Page): Promise<string> {
    const now = Math.floor(Date.now() / 30_000)
    const step = Math.max(now, this.last + 1)
    if (step > now + 1) await page.waitForTimeout((step - 1) * 30_000 - Date.now() + 200)
    this.last = step
    return totpCode(this.secret, step)
  }
}

// readQR reads the QR code the page shows with jsQR, a decoder that has
// nothing to do with the encoder in internal/qr: the page draws the SVG
// on a canvas and hands over the pixels.
async function readQR(page: Page): Promise<string> {
  const { data, size } = await page.getByTestId('totp-qr').evaluate(async (svg) => {
    const size = 416
    const img = new Image()
    img.src = `data:image/svg+xml;charset=utf-8,${encodeURIComponent(new XMLSerializer().serializeToString(svg))}`
    await img.decode()
    const canvas = document.createElement('canvas')
    canvas.width = canvas.height = size
    const ctx = canvas.getContext('2d')!
    ctx.fillStyle = '#fff'
    ctx.fillRect(0, 0, size, size)
    ctx.drawImage(img, 0, 0, size, size)
    return { data: Array.from(ctx.getImageData(0, 0, size, size).data), size }
  })
  const code = jsQR(new Uint8ClampedArray(data), size, size)
  if (!code) throw new Error('the QR code on the page cannot be read')
  return code.data
}

// enableTOTP turns two-step login on through the settings and returns the
// authenticator and the recovery codes.
async function enableTOTP(page: Page, user: string): Promise<{ auth: Authenticator; codes: string[] }> {
  await page.getByTestId('totp-on').click()
  const dialog = page.getByRole('dialog')
  await dialog.getByLabel('Your account password').fill(PASSWORD)
  await dialog.getByTestId('dialog-submit').click()

  // The QR code holds the otpauth URI with the same key the page shows
  // as text for typing.
  const uri = new URL(await readQR(page))
  await dialog.getByText('Cannot scan it?').click()
  const typed = (await dialog.getByTestId('totp-secret').innerText()).trim()
  expect(uri.protocol).toBe('otpauth:')
  expect(uri.host).toBe('totp')
  expect(decodeURIComponent(uri.pathname)).toBe(`/mail.robinmail.test:${user}`)
  expect(uri.searchParams.get('secret')).toBe(typed)
  expect(uri.searchParams.get('issuer')).toBe('mail.robinmail.test')
  expect([uri.searchParams.get('algorithm'), uri.searchParams.get('digits'), uri.searchParams.get('period')]).toEqual(['SHA1', '6', '30'])

  const auth = new Authenticator(typed)
  // A wrong code first: nothing is turned on.
  await dialog.getByRole('textbox', { name: 'Code', exact: true }).fill('000000')
  await dialog.getByTestId('dialog-submit').click()
  await expect(dialog.getByRole('alert')).toContainText('That code is not right')
  await dialog.getByRole('textbox', { name: 'Code', exact: true }).fill(await auth.code(page))
  await dialog.getByTestId('dialog-submit').click()

  const list = dialog.getByTestId('recovery-codes').getByRole('listitem')
  await expect(list).toHaveCount(10)
  const codes = (await list.allInnerTexts()).map((c) => c.trim())
  for (const c of codes) expect(c).toMatch(/^([a-z2-7]{4}-){3}[a-z2-7]{4}$/)
  await dialog.getByTestId('dialog-done').click()
  await expect(page.getByTestId('totp-enabled')).toContainText('10 of 10 recovery codes')
  return { auth, codes }
}

async function logOut(page: Page): Promise<void> {
  await page.getByRole('link', { name: 'Back to the mail' }).click()
  await page.getByTestId('logout').click()
  await expect(page).toHaveURL(/\/login/)
}

test('app passwords: made once, listed with their use, revoked', async ({ page, baseURL }) => {
  const user = newUser()
  await logIn(page, user)
  await openSecurity(page)

  // Until there is one, the account password works for a mail program.
  await expect(page.getByTestId('password-opens-mail')).toBeVisible()
  expect(await basicAuth(baseURL!, user, PASSWORD)).toBe(200)

  await page.getByTestId('new-app-password').click()
  const dialog = page.getByRole('dialog')
  await dialog.getByLabel('Name').fill('Phone')
  // The wrong account password makes nothing.
  await dialog.getByLabel('Your account password').fill('not the password')
  await dialog.getByTestId('dialog-submit').click()
  await expect(dialog.getByRole('alert')).toContainText('That is not your password')
  await dialog.getByLabel('Your account password').fill(PASSWORD)
  await dialog.getByTestId('dialog-submit').click()

  const secret = (await dialog.getByTestId('app-secret').innerText()).trim()
  expect(secret).toMatch(/^([a-z2-7]{4}-){7}[a-z2-7]{4}$/)
  await expect(dialog).toContainText('port 993')
  await dialog.getByTestId('dialog-done').click()

  // It is in the list, without its secret, and the notice has changed:
  // the account password is for the webmail only now.
  const row = page.getByTestId('app-password')
  await expect(row).toHaveCount(1)
  await expect(row).toContainText('Phone')
  await expect(row).toContainText('Never used')
  await expect(page.locator('body')).not.toContainText(secret)
  await expect(page.getByTestId('password-closed-for-mail')).toBeVisible()
  expect(await basicAuth(baseURL!, user, PASSWORD)).toBe(401)
  expect(await basicAuth(baseURL!, user, secret)).toBe(200)
  expect(await basicAuth(baseURL!, user, secret.replaceAll('-', '').toUpperCase())).toBe(200)

  // The use shows after a reload.
  await page.reload()
  await expect(page.getByTestId('app-password')).toContainText('Last used')
  await expect(page.getByTestId('app-password')).toContainText('from 127.0.0.1')

  // One for reading only.
  await page.getByTestId('new-app-password').click()
  await dialog.getByLabel('Name').fill('Reader')
  await dialog.getByLabel('Sending mail (SMTP)').uncheck()
  await dialog.getByLabel('JMAP programs').uncheck()
  // The Angular webmail (phase 9) also has calendars and contacts.
  if (angular) await dialog.getByLabel('Calendars and contacts (CalDAV, CardDAV)').uncheck()
  await dialog.getByLabel('Your account password').fill(PASSWORD)
  await dialog.getByTestId('dialog-submit').click()
  const imapOnly = (await dialog.getByTestId('app-secret').innerText()).trim()
  await dialog.getByTestId('dialog-done').click()
  await expect(page.getByTestId('app-password').filter({ hasText: 'Reader' })).toContainText('For reading (IMAP).')
  expect(await basicAuth(baseURL!, user, imapOnly)).toBe(401) // JMAP is not in its scope

  // Revoke the first: it stops working at once.
  await page.getByRole('button', { name: 'Revoke Phone' }).click()
  await dialog.getByTestId('dialog-submit').click()
  await expect(page.getByTestId('app-password')).toHaveCount(1)
  expect(await basicAuth(baseURL!, user, secret)).toBe(401)
})

test('password change and sessions: the other browser is logged out', async ({ page, browser }) => {
  const user = newUser()
  await logIn(page, user)
  const other = await second(browser)
  await logIn(other, user)

  await openSecurity(page)
  const rows = page.getByTestId('session')
  await expect(rows).toHaveCount(2)
  await expect(rows.filter({ hasText: 'This browser' })).toHaveCount(1)
  await expect(rows.first()).toContainText('Chrome on')
  await expect(rows.first()).toContainText('with the password, from 127.0.0.1')

  // Log the other browser out from here.
  await page.getByTestId('revoke-others').click()
  await page.getByRole('dialog').getByTestId('dialog-submit').click()
  await expect(rows).toHaveCount(1)
  await other.reload()
  await expect(other).toHaveURL(/\/login/)

  // A new password: the wrong current one is refused, two different new
  // ones too.
  await logIn(other, user)
  await expect(page.getByTestId('session')).toHaveCount(1) // not reloaded yet
  const form = page.getByTestId('password-form')
  await form.getByLabel('Current password').fill('not the password')
  await form.getByLabel('New password', { exact: true }).fill('a brand new password')
  await form.getByLabel('New password, again').fill('a brand new password')
  await form.getByRole('button', { name: 'Change password' }).click()
  await expect(form.getByRole('alert')).toContainText('That is not your password')
  await form.getByLabel('Current password').fill(PASSWORD)
  await form.getByLabel('New password, again').fill('another one entirely')
  await form.getByRole('button', { name: 'Change password' }).click()
  await expect(form.getByRole('alert')).toContainText('not the same')
  await form.getByLabel('New password, again').fill('a brand new password')
  await form.getByRole('button', { name: 'Change password' }).click()
  await expect(page.getByText('Password changed', { exact: true })).toBeVisible()

  // This browser stays logged in; the other is out, and the old password
  // no longer works there.
  await expect(page.getByTestId('session')).toHaveCount(1)
  await other.reload()
  await expect(other).toHaveURL(/\/login/)
  await fillLogin(other, user, PASSWORD)
  await expect(other.getByRole('alert')).toContainText('Wrong address or password')
  await logIn(other, user, 'a brand new password')

  // Logging this browser out from its own list is a logout.
  await page.reload()
  await page.getByRole('button', { name: /\(this browser\)/ }).click()
  await page.getByRole('dialog').getByTestId('dialog-submit').click()
  await expect(page).toHaveURL(/\/login/)
  await other.context().close()
})

async function second(browser: Browser): Promise<Page> {
  const context = await browser.newContext()
  return context.newPage()
}

test('two-step login: set up with the QR code, log in with a code, a used code, a recovery code, turn off', async ({ page, baseURL }) => {
  test.setTimeout(180_000)
  const user = newUser()
  await logIn(page, user)
  await openSecurity(page)
  const { auth, codes } = await enableTOTP(page, user)

  // With two-step login the account password is no way in for a mail
  // program any more.
  expect(await basicAuth(baseURL!, user, PASSWORD)).toBe(401)
  await logOut(page)

  // A wrong password says what it always said.
  await fillLogin(page, user, 'not the password')
  await expect(page.getByRole('alert')).toContainText('Wrong address or password')
  await expect(page.getByTestId('code-form')).toHaveCount(0)

  // The right one asks for the code. Nothing is open yet.
  await fillLogin(page, user)
  const form = page.getByTestId('code-form')
  await expect(form).toBeVisible()
  expect((await page.request.get('/auth/session')).status()).toBe(401)
  // A reload stays at the second step.
  await page.reload()
  await expect(form).toBeVisible()
  await form.getByRole('textbox', { name: 'Code', exact: true }).fill('000000')
  await form.getByRole('button', { name: 'Log in' }).click()
  await expect(form.getByRole('alert')).toContainText('That code is not right')
  const used = await auth.code(page)
  await form.getByRole('textbox', { name: 'Code', exact: true }).fill(used)
  await form.getByRole('button', { name: 'Log in' }).click()
  await expect(page).toHaveURL(/\/mail\/m\d+$/)

  // The session says how it came about.
  await openSecurity(page)
  await expect(page.getByTestId('session').first()).toContainText('with the password and a code')
  await logOut(page)

  // The code that was just used does not work a second time.
  await fillLogin(page, user)
  await form.getByRole('textbox', { name: 'Code', exact: true }).fill(used)
  await form.getByRole('button', { name: 'Log in' }).click()
  await expect(form.getByRole('alert')).toContainText('That code is not right')
  // A recovery code does, once.
  await form.getByTestId('toggle-recovery').click()
  await form.getByLabel('Recovery code').fill(codes[0]!)
  await form.getByRole('button', { name: 'Log in' }).click()
  await expect(page).toHaveURL(/\/mail\/m\d+$/)
  await openSecurity(page)
  await expect(page.getByTestId('totp-enabled')).toContainText('9 of 10 recovery codes')
  await logOut(page)
  await fillLogin(page, user)
  await form.getByTestId('toggle-recovery').click()
  await form.getByLabel('Recovery code').fill(codes[0]!)
  await form.getByRole('button', { name: 'Log in' }).click()
  await expect(form.getByRole('alert')).toContainText('not right, or was used before')
  // "Start over" goes back to the password.
  await form.getByRole('button', { name: 'Start over' }).click()
  await expect(page.getByTestId('login-form')).toBeVisible()

  // In with a code, and off again: that takes the password and a code.
  await fillLogin(page, user)
  await form.getByRole('textbox', { name: 'Code', exact: true }).fill(await auth.code(page))
  await form.getByRole('button', { name: 'Log in' }).click()
  await expect(page).toHaveURL(/\/mail\/m\d+$/)
  await openSecurity(page)
  await page.getByTestId('totp-off').click()
  const dialog = page.getByRole('dialog')
  await dialog.getByLabel('Your account password').fill(PASSWORD)
  await dialog.getByRole('textbox', { name: 'Code', exact: true }).fill('000000')
  await dialog.getByTestId('dialog-submit').click()
  await expect(dialog.getByRole('alert')).toContainText('That code is not right')
  await dialog.getByRole('textbox', { name: 'Code', exact: true }).fill(await auth.code(page))
  await dialog.getByTestId('dialog-submit').click()
  await expect(page.getByTestId('totp-on')).toBeVisible()
  await logOut(page)
  await logIn(page, user) // one step again
})

test('log in through a provider', async ({ page, request }) => {
  // The instance of "make e2e" has the fake provider of
  // contrib/dev/oidc-provider, which logs the development user in.
  const config = (await (await request.get('/auth/config')).json()) as { providers: { name: string; label: string }[] }
  test.skip(!config.providers.some((p) => p.name === 'test'), 'this instance has no provider "test" (see contrib/dev/e2e.sh)')

  await page.goto('/mail')
  await expect(page).toHaveURL(/\/login/)
  const button = page.getByTestId('provider-test')
  await expect(button).toHaveText('Log in with Test provider')
  await button.click()
  // To the provider and back, and on into the mail.
  await expect(page).toHaveURL(/\/mail\/m\d+$/)
  await expect(page.getByText(USER).first()).toBeVisible()

  await openSecurity(page)
  await expect(page.getByTestId('session').filter({ hasText: 'This browser' })).toContainText('through test')
  // What the page says about mail programs: they need an app password.
  await expect(page.getByTestId('section-apps')).toContainText('cannot do two-step login or log in through a provider')

  // The flow cookie is gone, and the session cookie is the strict one.
  const cookies = await page.context().cookies()
  expect(cookies.map((c) => c.name).sort()).toEqual(['robinmail'])
  expect(cookies[0]).toMatchObject({ httpOnly: true, sameSite: 'Strict' })
  await logOut(page)

  // A callback that no login in this browser started does nothing.
  await page.goto('/auth/oidc/test/callback?code=made-up&state=made-up')
  await expect(page).toHaveURL(/\/login\?error=oidc_failed/)
  await expect(page.getByRole('alert')).toContainText('did not work')
})
