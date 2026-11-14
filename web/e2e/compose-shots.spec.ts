// Not a test of behaviour: screenshots of writing mail, for a person to
// look at (desktop and phone, light and dark). Run with SHOTS=<dir>.
import { test } from '@playwright/test'
import { OTHER, cleanUp, destroyBySubject, login } from './dev'

const dir = process.env.SHOTS
test.skip(!dir, 'set SHOTS=<dir> to take screenshots')

const sizes = { desktop: { width: 1440, height: 900 }, phone: { width: 390, height: 844 } }

for (const [size, viewport] of Object.entries(sizes)) {
  for (const scheme of ['light', 'dark'] as const) {
    test(`compose ${size} ${scheme}`, async ({ browser }) => {
      const context = await browser.newContext({ viewport, colorScheme: scheme, hasTouch: size === 'phone' })
      const page = await context.newPage()
      const shot = (name: string) => page.screenshot({ path: `${dir}/c-${size}-${scheme}-${name}.png` })
      await login(page)
      await page.locator('[data-row]').first().waitFor()
      await page.getByTestId(size === 'phone' ? 'compose-small' : 'compose').click()
      const composer = page.getByTestId('composer').last()
      await composer.getByLabel('To', { exact: true }).fill('Ann Smith <ann@example.org>, other@robinmail.test, not an address')
      await composer.getByLabel('To', { exact: true }).press('Enter')
      await composer.getByLabel('Subject').fill('Plans for the week')
      await composer.getByTestId('plain-body').fill('Hello Ann,\n\nHere is the plan.\n\nRegards')
      await composer.getByTestId('composer-file').setInputFiles([
        { name: 'plan.txt', mimeType: 'text/plain', buffer: Buffer.from('the plan') },
        { name: 'a-rather-long-file-name-for-a-photo-of-the-harbour.png', mimeType: 'image/png', buffer: Buffer.alloc(40_000) },
      ])
      await page.waitForTimeout(2500)
      await shot('1-plain')
      await composer.getByTestId('composer-rich').click()
      await composer.getByTestId('rich-body').click()
      await page.keyboard.press('ControlOrMeta+a')
      await composer.getByRole('button', { name: 'Bold' }).click()
      await page.keyboard.press('ArrowRight')
      await page.keyboard.press('Enter')
      await composer.getByRole('button', { name: 'Bold' }).click()
      await composer.getByRole('button', { name: 'Bulleted list' }).click()
      await page.keyboard.type('one')
      await page.keyboard.press('Enter')
      await page.keyboard.type('two')
      await page.waitForTimeout(300)
      await shot('2-rich')
      if (size === 'desktop') {
        await page.getByTestId('compose').click()
        await page.waitForTimeout(300)
        await shot('3-two-open')
        await page.getByTestId('composer').last().getByTestId('composer-discard').click()
        await composer.getByRole('button', { name: /Minimise/ }).first().click()
        await page.waitForTimeout(300)
        await shot('4-minimised')
        await composer.getByRole('button', { name: /^Open/ }).first().click()
      }
      // A reply.
      await composer.getByTestId('composer-discard').click()
      await composer.getByTestId('composer-discard').click()
      await page.getByRole('link', { name: /Lunch/ }).first().click()
      await page.getByTestId('text-view').first().waitFor()
      await page.waitForTimeout(300)
      await shot('5-thread')
      await page.getByTestId('reply-all').click()
      await page.waitForTimeout(400)
      await shot('6-reply')
      await page.getByTestId('composer').last().getByTestId('composer-discard').click()
      // A message that was sent: one recipient gets it, one does not exist.
      const subject = `Shots sent ${size} ${scheme} ${Date.now()}`
      // On a phone the list, with its button for a new message, is behind the thread.
      if (size === 'phone') await page.getByRole('button', { name: 'Back to the list' }).click()
      await page.getByTestId(size === 'phone' ? 'compose-small' : 'compose').click()
      const c = page.getByTestId('composer').last()
      await c.getByLabel('To', { exact: true }).fill(`${OTHER}, nobody-${Date.now()}@robinmail.test, far@away.example`)
      await c.getByLabel('Subject').fill(subject)
      await c.getByTestId('rich-body').click()
      await page.keyboard.type('Who gets this?')
      await c.getByTestId('composer-send').click()
      await page.getByText('Message sent', { exact: true }).waitFor()
      await page.waitForTimeout(700)
      await shot('6b-sent-notice')
      if (size === 'desktop') await page.getByRole('button', { name: 'Close the conversation' }).click()
      await page.goto(page.url().replace(/\/mail\/.*/, '/mail/m3'))
      await page.getByRole('link', { name: new RegExp(subject) }).click()
      await page.locator('[data-delivery="Failed"]').waitFor({ timeout: 40_000 })
      await page.locator('article[data-email]').nth(1).waitFor({ timeout: 40_000 })
      await page.waitForTimeout(500)
      await shot('6c-delivery')
      await cleanUp(page, subject)
      // Identities.
      await page.goto('/settings')
      await page.getByTestId('identity').first().waitFor()
      await shot('7-identities')
      await page.getByTestId('add-identity').click()
      await page.waitForTimeout(400)
      await shot('8-add-identity')
      await destroyBySubject(page, 'no such subject', ['Re: Lunch on Tuesday'])
      const other = await (await browser.newContext()).newPage()
      await login(other, OTHER)
      await cleanUp(other, subject)
      await context.close()
    })
  }
}
