// Not a test of behaviour: it takes the screenshots a person looks at
// (desktop and phone, light and dark). Run with SHOTS=<dir>.
import { test } from '@playwright/test'
import { deliver, destroyBySubject, htmlMessage, login } from './dev'

const dir = process.env.SHOTS
test.skip(!dir, 'set SHOTS=<dir> to take screenshots')

const sizes = { desktop: { width: 1440, height: 900 }, phone: { width: 390, height: 844 } }

for (const [size, viewport] of Object.entries(sizes)) {
  for (const scheme of ['light', 'dark'] as const) {
    test(`${size} ${scheme}`, async ({ browser }) => {
      const subject = `Shots ${size} ${scheme} ${Date.now()}`
      deliver(htmlMessage(subject))
      const context = await browser.newContext({ viewport, colorScheme: scheme, hasTouch: size === 'phone' })
      const page = await context.newPage()
      const shot = (name: string) => page.screenshot({ path: `${dir}/${size}-${scheme}-${name}.png` })
      await page.goto('/login')
      await page.waitForSelector('form')
      await shot('1-login')
      await login(page)
      await page.locator('[data-row]').first().waitFor()
      await shot('2-list')
      await page.getByRole('link', { name: new RegExp(subject) }).click()
      await page.getByTestId('html-view').waitFor()
      await page.waitForTimeout(600)
      await shot('3-thread-html')
      await page.getByTestId('thread-move').click()
      await page.waitForTimeout(300)
      await shot('4-move-menu')
      await page.keyboard.press('Escape')
      if (size === 'phone') {
        await page.getByRole('button', { name: 'Back to the list' }).click()
        await page.getByRole('button', { name: /sidebar|menu/i }).first().click()
        await page.waitForTimeout(400)
        await shot('5-drawer')
        await page.keyboard.press('Escape')
      }
      await page.getByRole('link', { name: /Lunch/ }).first().click()
      await page.getByTestId('text-view').first().waitFor()
      await page.waitForTimeout(300)
      await shot('6-thread-text')
      await page.getByTestId('thread-labels').click()
      await page.waitForTimeout(300)
      await shot('6b-labels')
      await page.keyboard.press('Escape')
      if (size === 'desktop') {
        await page.getByTestId('thread-flag').click()
        await page.locator('[data-row]').nth(0).getByRole('checkbox').click()
        await page.locator('[data-row]').nth(1).getByRole('checkbox').click()
        await page.waitForTimeout(300)
        await shot('6c-selection-flag')
        await page.getByTestId('thread-flag').click()
        await page.keyboard.press('Escape')
        await page.getByTestId('filter').click()
        await page.waitForTimeout(300)
        await shot('7-filter')
        await page.keyboard.press('Escape')
        await page.getByRole('button', { name: 'New mailbox' }).click()
        await page.waitForTimeout(400)
        await shot('8-new-mailbox')
      }
      await page.keyboard.press('Escape')
      await destroyBySubject(page, subject, ['Re: Lunch on Tuesday'])
      await context.close()
    })
  }
}
