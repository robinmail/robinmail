// Phase 8: the Filters page. Rules are made in the page, kept as the
// account's Sieve script on the server, and run on mail that arrives over
// SMTP. The script can be read and written as text beside the rules.
import { expect, test, type Page } from '@playwright/test'
import { USER, cleanUp, jmap, login, smtpDeliver } from './dev'

const row = (page: Page, subject: string) => page.locator('[data-row]', { hasText: subject })

function message(subject: string): string {
  return `From: Billing <billing@shop.example>
To: ${USER}
Subject: ${subject}
Date: ${new Date().toUTCString().replace('GMT', '+0000')}
Message-ID: <${subject.replace(/[^a-z0-9]/gi, '')}@shop.example>

Please pay.
`
}

// The account's scripts as they were, so the test can put them back.
async function scripts(page: Page): Promise<{ id: string; name: string; blobId: string; isActive: boolean }[]> {
  const [got] = await jmap(page, [['SieveScript/get', { ids: null }]])
  return got!.list
}

test('make a rule, see it run on new mail, and edit the script by hand', async ({ page }) => {
  const stamp = Date.now()
  const box = `E2E filed ${stamp}`
  const subject = `E2E filter ${stamp}`
  await login(page)
  await expect(page.getByTestId('compose')).toBeEnabled()
  const before = await scripts(page)
  test.skip(before.length > 0, 'the dev account already has Sieve scripts; this test would change them')
  const [made] = await jmap(page, [['Mailbox/set', { create: { m: { name: box, parentId: null } } }]])
  const boxId: string = made!.created.m.id

  try {
    await page.getByRole('link', { name: 'Identities and signatures' }).click()
    await page.getByRole('link', { name: 'Filters' }).click()
    await expect(page).toHaveURL(/\/settings\/filters$/)
    await expect(page.getByText('No rules yet.')).toBeVisible()

    // A rule that is not complete says what is missing.
    await page.getByTestId('add-rule').click()
    await page.getByRole('button', { name: 'Add', exact: true }).click()
    await expect(page.getByRole('alert')).toContainText('Give the rule a name.')

    // From contains billing@ and Subject contains the stamp: move, read.
    await page.getByLabel('Name').fill('Invoices')
    await page.getByLabel('Text of condition 1').fill('billing@')
    await page.getByRole('button', { name: 'Another condition' }).click()
    await page.getByLabel('Text of condition 2').fill(subject)
    await page.getByLabel('Move to', { exact: true }).check()
    await page.getByLabel('Mailbox to move to').click()
    await page.getByRole('option', { name: box }).click()
    await page.getByLabel('Mark as read').check()
    await page.getByRole('button', { name: 'Add', exact: true }).click()
    const rule = page.getByTestId('rule')
    await expect(rule).toHaveCount(1)
    await expect(rule).toContainText('Invoices')
    await expect(rule).toContainText(`If From contains “billing@” and Subject contains “${subject}”`)
    await expect(rule).toContainText(`mark as read, move to ${box}`)
    await expect(page.getByTestId('filters-enabled')).toBeChecked()

    // On the server it is one active Sieve script.
    const now = await scripts(page)
    expect(now).toHaveLength(1)
    expect(now[0]).toMatchObject({ name: 'Filters', isActive: true })

    // The script, as text.
    await page.getByTestId('tab-script').click()
    const editor = page.getByTestId('script-editor')
    await expect(editor).toHaveValue(new RegExp(`fileinto :create "${box}";`))
    await expect(editor).toHaveValue(/^# robinmail rules 1\n/)

    // Mail over SMTP: the one that matches is filed and read, the other is not.
    expect(await smtpDeliver('billing@shop.example', USER, message(`${subject} matched`))).toMatch(/^250 /)
    expect(await smtpDeliver('other@shop.example', USER, message(`${subject} kept`).replace('billing@', 'other@'))).toMatch(/^250 /)
    await expect(async () => {
      const [, got] = await jmap(page, [
        ['Email/query', { filter: { subject } }],
        ['Email/get', { '#ids': { resultOf: 'c0', name: 'Email/query', path: '/ids' }, properties: ['subject', 'mailboxIds', 'keywords'] }],
      ])
      const bySubject = Object.fromEntries(got!.list.map((e: any) => [e.subject, e]))
      expect(Object.keys(bySubject[`${subject} matched`].mailboxIds)).toEqual([boxId])
      expect(bySubject[`${subject} matched`].keywords).toEqual({ $seen: true })
      expect(Object.keys(bySubject[`${subject} kept`].mailboxIds)).not.toContain(boxId)
    }).toPass({ timeout: 15_000 })

    // The mailbox in the webmail shows it.
    await page.getByRole('link', { name: 'Back to the mail' }).click()
    await page.locator(`a[href$="/mail/${boxId}"]`).click()
    await expect(row(page, `${subject} matched`)).toBeVisible()
    await page.goto('/settings/filters')

    // A script that does not parse is refused with the place of the fault.
    await page.getByTestId('tab-script').click()
    await page.getByTestId('script-editor').fill('if header :contains "subject" { discard; }\n')
    await page.getByTestId('save-script').click()
    await expect(page.getByTestId('filters-error')).toContainText('line 1')
    expect(await scripts(page)).toEqual(now) // nothing changed on the server

    // A script written by hand is saved, runs, and is not shown as rules.
    const hand = `require "fileinto";\n# by hand\nif header :contains "subject" "${subject} by hand" { fileinto "${box}"; }\n`
    await page.getByTestId('script-editor').fill(hand)
    await page.getByTestId('save-script').click()
    await expect(page.getByText('Script saved', { exact: true })).toBeVisible()
    await expect(page.getByTestId('filters-error')).toHaveCount(0)
    await page.getByTestId('tab-rules').click()
    await expect(page.getByTestId('custom-script')).toBeVisible()
    expect(await smtpDeliver('billing@shop.example', USER, message(`${subject} by hand`))).toMatch(/^250 /)
    await expect(async () => {
      const [, got] = await jmap(page, [
        ['Email/query', { filter: { subject: `${subject} by hand` } }],
        ['Email/get', { '#ids': { resultOf: 'c0', name: 'Email/query', path: '/ids' }, properties: ['mailboxIds'] }],
      ])
      expect(Object.keys(got!.list[0].mailboxIds)).toEqual([boxId])
    }).toPass({ timeout: 15_000 })

    // Back to rules: an empty list, then off.
    await page.getByRole('button', { name: 'Replace it with rules' }).click()
    await expect(page.getByText('No rules yet.')).toBeVisible()
    await page.getByTestId('filters-enabled').click()
    await expect(page.getByText('The filters are off')).toBeVisible()
    expect((await scripts(page))[0]).toMatchObject({ isActive: false })
    await page.reload()
    await expect(page.getByText('The filters are off')).toBeVisible()
  } finally {
    // Leave the account as it was: no script, no mailbox, no mail.
    const left = await scripts(page)
    await jmap(page, [['SieveScript/set', { onSuccessDeactivateScript: true }]])
    await jmap(page, [['SieveScript/set', { destroy: left.map((s) => s.id) }]])
    await cleanUp(page, subject)
    await jmap(page, [['Mailbox/set', { destroy: [boxId], onDestroyRemoveEmails: true }]])
  }
})
