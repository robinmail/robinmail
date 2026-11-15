// Phase 9: the contacts of the Angular webmail (JMAP for Contacts, RFC
// 9610), and the address book as a .vcf over CardDAV.
import { expect, type Page, test } from '@playwright/test'
import { angular, jmap, login } from './dev'

test.skip(!angular, 'the contacts are in the Angular webmail only')

async function removeCards(page: Page, text: string): Promise<void> {
  await jmap(page, [
    ['ContactCard/query', { filter: { text } }],
    ['ContactCard/set', { '#destroy': { resultOf: 'c0', name: 'ContactCard/query', path: '/ids' } }],
  ])
}

test('a contact: made, found, changed, downloaded as vCard, deleted', async ({ page }) => {
  const surname = `Tester${Date.now()}`
  await login(page)
  try {
    await page.getByTestId('app-contacts').click()
    await expect(page).toHaveURL(/\/contacts$/)
    await page.getByTestId('new-contact').click()
    await page.getByTestId('contact-given').fill('Ada')
    await page.getByTestId('contact-surname').fill(surname)
    await page.getByTestId('contact-org').fill('Analytical Engines')
    await page.getByTestId('contact-email-0').fill(`ada.${surname.toLowerCase()}@example.org`)
    await page.getByTestId('contact-phone-0').fill('+44 20 7946 0000')
    await page.getByTestId('contact-save').click()
    await expect(page.getByTestId('contact-detail')).toContainText(`Ada ${surname}`)

    await page.getByTestId('contact-search').fill(surname.slice(0, 8))
    await expect(page.getByTestId('contact-row').filter({ hasText: surname })).toHaveCount(1)
    await page.getByTestId('contact-search').fill('nobody-by-this-name')
    await expect(page.getByTestId('contact-row')).toHaveCount(0)
    await page.getByTestId('contact-search').fill('')

    await page.getByTestId('contact-row').filter({ hasText: surname }).click()
    await page.getByTestId('contact-edit').click()
    await page.getByTestId('contact-given').fill('Augusta Ada')
    await page.getByTestId('contact-save').click()
    await expect(page.getByTestId('contact-detail')).toContainText(`Augusta Ada ${surname}`)

    const [, got] = await jmap(page, [
      ['ContactCard/query', { filter: { 'name/surname': surname } }],
      ['ContactCard/get', { '#ids': { resultOf: 'c0', name: 'ContactCard/query', path: '/ids' } }],
    ])
    expect(got!.list).toHaveLength(1)
    expect(Object.values(got!.list[0].phones)[0]).toMatchObject({ number: '+44 20 7946 0000' })

    // The address book as one file, through CardDAV with the session.
    const [books] = await jmap(page, [['AddressBook/get', {}]])
    const session = await (await page.request.get('/.well-known/jmap')).json()
    const me = session.primaryAccounts['urn:ietf:params:jmap:contacts']
    const vcf = await (await page.request.get(`/dav/addressbooks/${me}/${books!.list[0].id}/`)).text()
    expect(vcf).toContain(`FN:Augusta Ada ${surname}`)
    expect(vcf).toContain(`N:${surname};Augusta Ada;;;`)

    await page.getByTestId('contact-edit').click()
    page.once('dialog', (d) => d.accept())
    await page.getByTestId('contact-delete').click()
    await expect(page.getByTestId('contact-row').filter({ hasText: surname })).toHaveCount(0)
  } finally {
    await removeCards(page, surname)
  }
})

test('import a .vcf file', async ({ page }) => {
  const surname = `Imported${Date.now()}`
  await login(page)
  try {
    await page.goto('/contacts')
    const vcf = `BEGIN:VCARD\r\nVERSION:3.0\r\nN:${surname};Grace;;;\r\nFN:Grace ${surname}\r\nEMAIL;TYPE=INTERNET:grace@example.org\r\nEND:VCARD\r\n` +
      `BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alan ${surname}\r\nUID:urn:uuid:${crypto.randomUUID()}\r\nEND:VCARD\r\n`
    await page.getByTestId('import-vcf').setInputFiles({ name: 'cards.vcf', mimeType: 'text/vcard', buffer: Buffer.from(vcf) })
    await expect(page.getByText('2 contacts imported')).toBeVisible()
    await expect(page.getByTestId('contact-row').filter({ hasText: surname })).toHaveCount(2)
  } finally {
    await removeCards(page, surname)
  }
})
