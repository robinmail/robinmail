// Phase 8: the Vacation page. The automatic reply is JMAP's
// VacationResponse; the server sends it for mail that arrives over SMTP.
import { expect, test } from '@playwright/test'
import { OTHER, USER, cleanUp, cli, jmap, login, smtpDeliver } from './dev'

test('turn the automatic reply on, get one, turn it off', async ({ page, browser }) => {
  const stamp = Date.now()
  const subject = `E2E away ${stamp}`
  // The server answers each sender once a week, so every run writes from
  // an address of its own: an alias of the other user, where the reply
  // then arrives.
  const sender = `vacation${stamp}@robinmail.test`
  cli('alias', 'add', sender, OTHER)
  await login(page)
  await expect(page.getByTestId('compose')).toBeEnabled()
  const [was] = await jmap(page, [['VacationResponse/get', { ids: ['singleton'] }]])
  const before = was!.list[0]

  try {
    await page.getByRole('link', { name: 'Identities and signatures' }).click()
    await page.getByRole('link', { name: 'Vacation' }).click()
    await expect(page).toHaveURL(/\/settings\/vacation$/)
    await expect(page.getByTestId('vacation-save')).toBeDisabled()

    // Dates the wrong way round are refused before anything is sent.
    await page.getByTestId('vacation-enabled').click()
    await page.getByLabel('From', { exact: true }).fill('2031-01-10T09:00')
    await page.getByLabel('Until').fill('2031-01-02T09:00')
    await page.getByTestId('vacation-save').click()
    await expect(page.getByRole('alert')).toContainText('must be after')

    await page.getByLabel('From', { exact: true }).fill('')
    await page.getByLabel('Until').fill('2031-01-02T09:00')
    await page.getByLabel('Subject').fill(subject)
    await page.getByLabel('Message').fill('I am away until January.\nDev')
    await page.getByTestId('vacation-save').click()
    await expect(page.getByText('The automatic reply is on', { exact: true })).toBeVisible()
    const [now] = await jmap(page, [['VacationResponse/get', { ids: ['singleton'] }]])
    expect(now!.list[0]).toMatchObject({ isEnabled: true, fromDate: null, subject, textBody: 'I am away until January.\nDev' })
    expect(now!.list[0].toDate).toMatch(/^2031-01-0[12]T\d\d:00:00Z$/)
    // A reload shows what was saved.
    await page.reload()
    await expect(page.getByLabel('Subject')).toHaveValue(subject)
    await expect(page.getByTestId('vacation-enabled')).toBeChecked()

    // A message over SMTP that names the user gets the reply...
    const question = `E2E question ${stamp}`
    expect(await smtpDeliver(sender, USER, `From: Other <${sender}>\nTo: ${USER}\nSubject: ${question}\nMessage-ID: <q${stamp}@robinmail.test>\n\nAre you there?\n`)).toMatch(/^250 /)
    // ...and one from a list does not (RFC 3834).
    const list = `vacationlist${stamp}@robinmail.test`
    cli('alias', 'add', list, OTHER)
    expect(await smtpDeliver(list, USER, `From: List <${list}>\nTo: ${USER}\nSubject: ${question} digest\nList-Id: <dev.lists.example>\n\nNews.\n`)).toMatch(/^250 /)

    const other = await (await browser.newContext()).newPage()
    await login(other, OTHER)
    await expect(async () => {
      const [, got] = await jmap(other, [
        ['Email/query', { filter: { subject } }],
        ['Email/get', { '#ids': { resultOf: 'c0', name: 'Email/query', path: '/ids' },
          properties: ['subject', 'from', 'to', 'header:Auto-Submitted:asText', 'header:In-Reply-To:asMessageIds', 'preview'] }],
      ])
      expect(got!.list).toHaveLength(1)
      expect(got!.list[0]).toMatchObject({
        subject,
        from: [{ name: null, email: USER }],
        to: [{ name: null, email: sender }],
        'header:Auto-Submitted:asText': 'auto-replied',
        'header:In-Reply-To:asMessageIds': [`q${stamp}@robinmail.test`],
      })
      expect(got!.list[0].preview).toContain('I am away until January.')
    }).toPass({ timeout: 30_000 })
    // Both messages arrived as usual.
    await expect(async () => {
      const [got] = await jmap(page, [['Email/query', { filter: { subject: question } }]])
      expect(got!.ids).toHaveLength(2)
    }).toPass({ timeout: 10_000 })

    // Off again.
    await page.getByTestId('vacation-enabled').click()
    await page.getByTestId('vacation-save').click()
    await expect(page.getByText('The automatic reply is off', { exact: true })).toBeVisible()
    const [off] = await jmap(page, [['VacationResponse/get', { ids: ['singleton'] }]])
    expect(off!.list[0].isEnabled).toBe(false)

    await cleanUp(other, subject)
    cli('alias', 'remove', list)
  } finally {
    const { id: _id, ...rest } = before
    await jmap(page, [['VacationResponse/set', { update: { singleton: rest } }]])
    await cleanUp(page, `E2E question ${stamp}`)
    cli('alias', 'remove', sender)
  }
})
