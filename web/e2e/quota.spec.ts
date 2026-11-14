// Phase 8: quotas in the webmail. The sidebar shows how full the account
// is (JMAP Quota); a full account refuses mail at SMTP and takes it again
// once there is room.
import { expect, test } from '@playwright/test'
import { OTHER, cleanUp, cli, login, smtpDeliver } from './dev'

test('the sidebar shows the quota, and a full account refuses mail', async ({ page }) => {
  const stamp = Date.now()
  const subject = `E2E quota ${stamp}`
  // The other user of the seed, so the account the developer looks at
  // keeps its own limits.
  const has = Number(/messages\s+(\d+)/.exec(cli('user', 'quota', OTHER))![1])
  const message = (n: number) => `From: Sender <sender@example.org>\nTo: ${OTHER}\nSubject: ${subject} ${n}\n\nx\n`
  try {
    // No limit: no meter.
    cli('user', 'quota', OTHER, 'none', 'none')
    await login(page, OTHER)
    await expect(page.getByTestId('compose')).toBeEnabled()
    await expect(page.getByTestId('quota')).toHaveCount(0)

    // Room for two more messages, and 50 MB.
    cli('user', 'quota', OTHER, '50M', String(has + 2))
    await page.reload()
    const count = page.locator('[data-quota="count"]')
    await expect(count).toContainText(`${has} of ${has + 2} messages`)
    await expect(page.locator('[data-quota="octets"]')).toContainText('of 50 MB')
    await expect(count.getByRole('progressbar')).toHaveAttribute('aria-valuenow', String(Math.round((has / (has + 2)) * 100)))

    // Mail arrives and the meter follows without a reload.
    expect(await smtpDeliver('sender@example.org', OTHER, message(1))).toMatch(/^250 /)
    expect(await smtpDeliver('sender@example.org', OTHER, message(2))).toMatch(/^250 /)
    await expect(count).toContainText(`${has + 2} of ${has + 2} messages`, { timeout: 15_000 })
    await expect(count).toContainText('Full: new mail is refused')

    // Full: the next message is refused at RCPT, for now.
    expect(await smtpDeliver('sender@example.org', OTHER, message(3))).toMatch(/^452 4\.2\.2 /)

    // Deleting makes room, and mail is taken again.
    await cleanUp(page, `${subject} 1`)
    await expect(count).toContainText(`${has + 1} of ${has + 2} messages`, { timeout: 15_000 })
    expect(await smtpDeliver('sender@example.org', OTHER, message(3))).toMatch(/^250 /)
  } finally {
    cli('user', 'quota', OTHER, 'default', 'default')
    await cleanUp(page, subject)
  }
})
