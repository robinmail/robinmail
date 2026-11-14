// Not a test of behaviour: it takes the screenshots a person looks at, of
// the login page and the security settings. Run with SHOTS=<dir>.
import { execFileSync } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { fileURLToPath } from 'node:url'
import { test } from '@playwright/test'

const dir = process.env.SHOTS
test.skip(!dir, 'set SHOTS=<dir> to take screenshots')

const root = fileURLToPath(new URL('../..', import.meta.url))
const PASSWORD = 'e2e-shots-password'
const sizes = { desktop: { width: 1440, height: 900 }, phone: { width: 390, height: 844 } }

for (const [size, viewport] of Object.entries(sizes)) {
  for (const scheme of ['light', 'dark'] as const) {
    test(`security ${size} ${scheme}`, async ({ browser }) => {
      const user = `shots-${randomBytes(4).toString('hex')}@robinmail.test`
      execFileSync('dev/robinmail', ['-config', 'dev/robinmail.json', 'user', 'add', user], { cwd: root, input: `${PASSWORD}\n` })
      const context = await browser.newContext({ viewport, colorScheme: scheme, hasTouch: size === 'phone' })
      const page = await context.newPage()
      const shot = (name: string) => page.screenshot({ path: `${dir}/auth-${size}-${scheme}-${name}.png`, fullPage: true })
      await page.goto('/login')
      await page.getByTestId('login-form').waitFor()
      await page.waitForTimeout(300)
      await shot('1-login')
      await page.getByLabel('Email address').fill(user)
      await page.getByLabel('Password').fill(PASSWORD)
      await page.getByRole('button', { name: 'Log in', exact: true }).click()
      await page.waitForURL(/\/mail\//)
      await page.goto('/settings/security')
      await page.getByTestId('section-sessions').waitFor()
      await shot('2-security')

      await page.getByTestId('new-app-password').click()
      const dialog = page.getByRole('dialog')
      await dialog.getByLabel('Name').fill('Thunderbird on the laptop')
      await dialog.getByLabel('Your account password').fill(PASSWORD)
      await page.waitForTimeout(300)
      await shot('3-new-app-password')
      await dialog.getByTestId('dialog-submit').click()
      await dialog.getByTestId('app-secret').waitFor()
      await shot('4-app-secret')
      await dialog.getByTestId('dialog-done').click()

      await page.getByTestId('totp-on').click()
      await dialog.getByLabel('Your account password').fill(PASSWORD)
      await dialog.getByTestId('dialog-submit').click()
      await dialog.getByTestId('totp-qr').waitFor()
      await page.waitForTimeout(300)
      await shot('5-totp-scan')
      await page.keyboard.press('Escape')
      await page.waitForTimeout(300)
      await shot('6-security-with-app-password')
      await context.close()
    })
  }
}
