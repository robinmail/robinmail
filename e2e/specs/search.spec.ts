// Phase 8: the search box. Full-text search in the open mailbox through
// Email/query's text condition, with what matched marked in each row
// (SearchSnippet/get). Against the real binary and the dev instance.
import { expect, test, type Page } from '@playwright/test'
import { deliver, destroyBySubject, login } from './dev'

const row = (page: Page, subject: string) => page.locator('[data-row]', { hasText: subject })

function message(subject: string, body: string): string {
  return `From: Harbour Office <office@harbour.example>
To: dev@robinmail.test
Subject: ${subject}
Date: ${new Date().toUTCString().replace('GMT', '+0000')}
Message-ID: <${subject.replace(/[^a-z0-9]/gi, '')}@harbour.example>

${body}
`
}

test('search the bodies of a mailbox and see what matched', async ({ page }) => {
  const stamp = Date.now()
  const word = `zeewier${stamp}`
  const fee = `E2E search fee ${stamp}`
  const tour = `E2E search tour ${stamp}`
  deliver(message(fee, `Dear tenant,\n\nThe ${word} fee for <berth 12> is due on Friday.\n`))
  deliver(message(tour, `We start the tour at the quay. No ${word} this year, only kelp.\n`))
  deliver(message(`E2E search other ${stamp}`, 'Nothing of the kind in here.\n'))

  await login(page)
  await expect(row(page, fee)).toBeVisible()

  // A word that is only in bodies: two of the three, each with its snippet.
  const box = page.getByTestId('search')
  await box.fill(word)
  await expect(page).toHaveURL(new RegExp(`[?&]q=${word}`))
  await expect(page.locator('[data-row]')).toHaveCount(2)
  await expect(row(page, fee).getByTestId('snippet').locator('mark')).toHaveText(word)
  // The snippet is text: what looks like a tag in the message is shown, not made.
  await expect(row(page, fee).getByTestId('snippet')).toContainText(`${word} fee for <berth 12> is due`)
  await expect(row(page, tour).getByTestId('snippet').locator('mark')).toHaveText(word)

  // Every word is needed; quoted words are a phrase.
  await box.fill(`${word} friday`)
  await expect(page.locator('[data-row]')).toHaveCount(1)
  await expect(row(page, fee).getByTestId('snippet').locator('mark')).toHaveCount(2)
  await box.fill(`"${word} this year"`)
  await expect(page.locator('[data-row]')).toHaveCount(1)
  await expect(row(page, tour).getByTestId('snippet').locator('mark')).toHaveText(`${word} this year`)

  // A word of the subject is marked there.
  await box.fill(`tour ${stamp}`)
  await expect(row(page, tour).locator('mark').first()).toHaveText('tour')

  // The search is in the address: a reload keeps it, and opening a result keeps the list.
  await box.fill(word)
  await expect(page.locator('[data-row]')).toHaveCount(2)
  await page.reload()
  await expect(page.getByTestId('search')).toHaveValue(word)
  await expect(page.locator('[data-row]')).toHaveCount(2)
  await row(page, fee).getByRole('link').click()
  await expect(page.getByTestId('thread-subject')).toHaveText(fee)
  await expect(page.locator('[data-row]')).toHaveCount(2)

  // Nothing found, and back to the whole mailbox.
  await page.getByTestId('search').fill(`${word}nothing`)
  await expect(page.getByText('No messages match the search.')).toBeVisible()
  await page.getByTestId('search-clear').click()
  await expect(page).not.toHaveURL(/[?&]q=/)
  await expect(row(page, `E2E search other ${stamp}`)).toBeVisible()

  // "/" puts the cursor in the box.
  await page.keyboard.press('Escape')
  await page.locator('body').press('/')
  await expect(page.getByTestId('search')).toBeFocused()

  await destroyBySubject(page, `E2E search`)
})
