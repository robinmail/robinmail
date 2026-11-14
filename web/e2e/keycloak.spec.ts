// The OpenID Connect login against a real provider: the Keycloak of
// contrib/compose/keycloak.yaml, with the realm of
// contrib/compose/keycloak/robinmail-realm.json. "make keycloak-test"
// starts it; when it is not running this test skips itself.
//
// The password is that of the two users of that realm file, which exist
// in that container only.

import { expect, test, type Page } from '@playwright/test'
import { USER } from './dev'

const KEYCLOAK_PASSWORD = 'robinmail-keycloak-dev'

test.beforeEach(async ({ request }) => {
  const config = (await (await request.get('/auth/config')).json()) as { providers: { name: string }[] }
  test.skip(!config.providers.some((p) => p.name === 'keycloak'), 'Keycloak is not running (make keycloak-test starts it)')
})

async function keycloakLogin(page: Page, username: string): Promise<void> {
  await page.goto('/')
  const base = page.url()
  await expect(page).toHaveURL(/\/login/)
  await page.getByTestId('provider-keycloak').click()
  // Keycloak's own login page, on its own origin.
  await expect(page).toHaveURL(/127\.0\.0\.1:8180\/realms\/robinmail\/protocol\/openid-connect\/auth\?/)
  const url = new URL(page.url())
  console.log(`authorization request to Keycloak: ${[...url.searchParams.keys()].sort().join(', ')}`)
  expect(url.searchParams.get('response_type')).toBe('code')
  expect(url.searchParams.get('code_challenge_method')).toBe('S256')
  expect(url.searchParams.get('scope')).toBe('openid email')
  expect(url.searchParams.get('redirect_uri')).toBe(`${new URL(base).origin}/auth/oidc/keycloak/callback`)
  for (const p of ['state', 'nonce', 'code_challenge']) expect(url.searchParams.get(p)?.length).toBeGreaterThanOrEqual(43)
  await page.locator('#username').fill(username)
  await page.locator('#password').fill(KEYCLOAK_PASSWORD)
  await page.locator('#kc-login').click()
}

test('a Keycloak user logs in to the webmail', async ({ page }) => {
  await keycloakLogin(page, 'dev')
  await expect(page).toHaveURL(/127\.0\.0\.1:80\d\d\/mail\/m\d+$/)
  await expect(page.getByText(USER).first()).toBeVisible()
  const session = (await (await page.request.get('/auth/session')).json()) as { username: string; method: string }
  console.log(`logged in through Keycloak: ${JSON.stringify({ username: session.username, method: session.method })}`)
  expect(session).toMatchObject({ username: USER, method: 'oidc:keycloak' })

  await page.getByRole('link', { name: 'Password and security' }).click()
  await expect(page.getByTestId('session').filter({ hasText: 'This browser' })).toContainText('through keycloak')

  // Logged out of robinmail, the provider still knows the browser: the
  // button logs in again without a password. (robinmail does not log the
  // user out at the provider.)
  await page.getByRole('link', { name: 'Back to the mail' }).click()
  await page.getByTestId('logout').click()
  await expect(page).toHaveURL(/\/login/)
  await page.getByTestId('provider-keycloak').click()
  await expect(page).toHaveURL(/127\.0\.0\.1:80\d\d\/mail\/m\d+$/)
})

test('a Keycloak user without a mailbox does not get in', async ({ page }) => {
  await keycloakLogin(page, 'stranger')
  await expect(page).toHaveURL(/\/login\?error=oidc_no_account/)
  await expect(page.getByRole('alert')).toContainText('no mailbox here')
  expect((await page.request.get('/auth/session')).status()).toBe(401)
})
