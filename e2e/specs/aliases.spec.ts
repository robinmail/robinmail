// Phase 8: aliases in the webmail. An alias that delivers to the account
// can be an identity: the identities page offers it, a message goes out
// in its name, and mail for the alias arrives.
import { expect, test, type Locator, type Page } from '@playwright/test'
import { OTHER, USER, cleanUp, cli, jmap, login } from './dev'

const composer = (page: Page) => page.getByTestId('composer').last()
const row = (page: Page, subject: string) => page.locator('[data-row]', { hasText: subject })

async function arrived(page: Page, subject: string): Promise<Locator> {
  await expect(async () => {
    await page.getByRole('button', { name: 'Check for new mail' }).click()
    await expect(row(page, subject)).toBeVisible({ timeout: 2000 })
  }).toPass({ timeout: 40_000 })
  return row(page, subject)
}

test('an alias as an identity: offered, sent from, and mail for it arrives', async ({ browser }) => {
  const stamp = Date.now()
  const alias = `office${stamp}@robinmail.test`
  const subject = `E2E alias ${stamp}`
  cli('alias', 'add', alias, USER)
  try {
    const context = await browser.newContext()
    const page = await context.newPage()
    await login(page)
    await expect(page.getByTestId('compose')).toBeEnabled()

    // The identities page offers the alias next to the account's own
    // address, and it needs no tag.
    await page.getByRole('link', { name: 'Identities and signatures' }).click()
    await page.getByTestId('add-identity').click()
    await page.getByTestId('identity-base').click()
    await page.getByRole('option', { name: alias }).click()
    await page.getByLabel('Name').fill('The Office')
    await page.getByRole('button', { name: 'Add', exact: true }).click()
    await expect(page.getByTestId('identity').filter({ hasText: alias })).toContainText('The Office')
    await page.getByRole('link', { name: 'Back to the mail' }).click()

    // A message from the alias to the other user...
    await page.keyboard.press('c')
    const c = composer(page)
    await c.getByLabel('From').click()
    await page.getByRole('option', { name: new RegExp(alias) }).click()
    await c.getByLabel('To', { exact: true }).fill(OTHER)
    await c.getByLabel('Subject').fill(subject)
    await c.getByTestId('plain-body').fill('From the office.')
    await c.getByTestId('composer-send').click()

    // ...arrives there with the alias as its author,
    const other = await (await browser.newContext()).newPage()
    await login(other, OTHER)
    await (await arrived(other, subject)).getByRole('link').click()
    await expect(other.getByTestId('thread-subject')).toHaveText(subject)
    const [, got] = await jmap(other, [
      ['Email/query', { filter: { subject } }],
      ['Email/get', { '#ids': { resultOf: 'c0', name: 'Email/query', path: '/ids' }, properties: ['from'] }],
    ])
    expect(got!.list[0].from).toEqual([{ name: 'The Office', email: alias }])

    // ...and the answer, written to the alias, comes back to the account.
    await other.keyboard.press('r')
    const reply = composer(other)
    await expect(reply.getByTestId('address-chip')).toHaveText(['The Office'])
    await reply.getByTestId('plain-body').fill('Thank you, office.')
    await reply.getByTestId('composer-send').click()
    await arrived(page, `Re: ${subject}`)

    // Clean up: the identity and the mail on both sides.
    await page.getByRole('link', { name: 'Identities and signatures' }).click()
    await page.getByRole('button', { name: `Remove ${alias}` }).click()
    await page.getByRole('button', { name: 'Remove', exact: true }).click()
    await expect(page.getByTestId('identity').filter({ hasText: alias })).toHaveCount(0)
    await cleanUp(page, subject)
    await cleanUp(other, subject)
  } finally {
    cli('alias', 'remove', alias)
  }
})
