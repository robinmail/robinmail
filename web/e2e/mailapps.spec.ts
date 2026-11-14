// Phase 8: what a mail program needs. The Security page shows the
// settings and offers the profile for Apple devices; Thunderbird's
// autoconfig file is served for the domain.
import { readFileSync } from 'node:fs'
import { expect, test } from '@playwright/test'
import { USER, login } from './dev'

test('the security page shows the mail settings and downloads the Apple profile', async ({ page }) => {
  await login(page)
  await page.goto('/settings/security')
  const section = page.getByTestId('section-mail-apps')
  await expect(section).toBeVisible()
  await expect(section.getByTestId('mail-settings')).toContainText(USER)
  await expect(section.getByTestId('mail-settings-imap').first()).toContainText(/port \d+, (TLS|STARTTLS)/)
  await expect(section.getByTestId('mail-settings-smtp').first()).toContainText(/port \d+, (TLS|STARTTLS)/)

  const [download] = await Promise.all([page.waitForEvent('download'), section.getByTestId('mobileconfig').click()])
  expect(download.suggestedFilename()).toBe('mail.mobileconfig')
  const profile = readFileSync(await download.path(), 'utf8')
  expect(profile).toContain('<string>com.apple.mail.managed</string>')
  expect(profile).toContain(`<key>EmailAddress</key>\n\t\t\t<string>${USER}</string>`)
  expect(profile).toContain('<string>EmailTypeIMAP</string>')
  expect(profile).not.toMatch(/<key>(Incoming|Outgoing)Password<\/key>/)

  // The file Thunderbird fetches, for the domain of the address. It needs
  // no login.
  const res = await page.request.get(`/mail/config-v1.1.xml?emailaddress=${encodeURIComponent(USER)}`)
  expect(res.status()).toBe(200)
  const xml = await res.text()
  expect(xml).toContain(`<domain>${USER.split('@')[1]}</domain>`)
  expect(xml).toContain('<incomingServer type="imap">')
  expect(xml).toContain('<outgoingServer type="smtp">')
  expect((await page.request.get('/mail/config-v1.1.xml?emailaddress=someone@elsewhere.example')).status()).toBe(404)
})
