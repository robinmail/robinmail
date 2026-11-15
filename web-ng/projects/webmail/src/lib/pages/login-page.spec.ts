import { provideZonelessChangeDetection } from '@angular/core'
import { TestBed } from '@angular/core/testing'
import { Router, provideRouter } from '@angular/router'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { auth } from '../app'
import { provideWebmail, webmailRoutes } from '../routes'
import { text, type, until } from '../testing/helpers'
import { LoginPage } from './login-page'

type Answer = { status: number; body?: unknown; headers?: Record<string, string> }

// A stand-in for the /auth endpoints: each path answers what the test says.
function server(answers: Record<string, Answer | ((body: unknown) => Answer)>) {
  const calls: { path: string; body: unknown }[] = []
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
    const path = String(input)
    const body = init?.body ? JSON.parse(String(init.body)) : undefined
    calls.push({ path, body })
    const a = answers[path] ?? { status: 404, body: { error: 'not found' } }
    const answer = typeof a === 'function' ? a(body) : a
    return new Response(answer.body === undefined ? null : JSON.stringify(answer.body), { status: answer.status, headers: { 'Content-Type': 'application/json', ...answer.headers } })
  })
  return calls
}

async function open(url = '/login') {
  TestBed.configureTestingModule({ providers: [provideZonelessChangeDetection(), provideRouter(webmailRoutes), provideWebmail()] })
  const router = TestBed.inject(Router)
  await router.navigateByUrl(url)
  const fixture = TestBed.createComponent(LoginPage)
  await fixture.whenStable()
  return { fixture, router, el: fixture.nativeElement as HTMLElement }
}

function submit(el: HTMLElement) {
  el.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }))
}

describe('the login page', () => {
  beforeEach(() => {
    auth.checked = true
    auth.account = null
    auth.waiting = false
  })
  afterEach(() => vi.unstubAllGlobals())

  it('shows a button for every provider the server has', async () => {
    server({ '/auth/config': { status: 200, body: { providers: [{ name: 'test', label: 'Test provider' }] } } })
    const { fixture, el } = await open()
    await until(() => !!el.querySelector('[data-testid="provider-test"]'), fixture)
    const a = el.querySelector<HTMLAnchorElement>('[data-testid="provider-test"]')!
    expect(text(a)).toBe('Log in with Test provider')
    expect(a.getAttribute('href')).toBe('/auth/oidc/test/start')
  })

  it('says what a provider login came back with', async () => {
    server({})
    const { el } = await open('/login?error=oidc_no_account')
    expect(text(el.querySelector('[role="alert"]'))).toBe('There is no mailbox here for the account you logged in with at the provider.')
  })

  it('logs in and goes where the address says, when that is a path of this app', async () => {
    const calls = server({ '/auth/login': { status: 200, body: { username: 'dev@robinmail.test', name: 'Dev' } } })
    const { fixture, router, el } = await open('/login?next=/settings')
    type(el.querySelector<HTMLInputElement>('input[name="username"]')!, ' dev@robinmail.test ')
    type(el.querySelector<HTMLInputElement>('input[name="password"]')!, 'secret')
    await fixture.whenStable()
    submit(el)
    await until(() => router.url === '/settings')
    expect(calls.find((c) => c.path === '/auth/login')!.body).toMatchObject({ username: 'dev@robinmail.test', password: 'secret' })
    expect(auth.account?.username).toBe('dev@robinmail.test')
  })

  it('says so when the password is wrong, and how long to wait after too many tries', async () => {
    let status = 401
    server({ '/auth/login': () => ({ status, body: { error: 'no' }, headers: { 'Retry-After': '120' } }) })
    const { fixture, el } = await open()
    type(el.querySelector<HTMLInputElement>('input[name="username"]')!, 'dev@robinmail.test')
    type(el.querySelector<HTMLInputElement>('input[name="password"]')!, 'wrong')
    await fixture.whenStable()
    submit(el)
    await until(() => !!el.querySelector('[role="alert"]'), fixture)
    expect(text(el.querySelector('[role="alert"]'))).toBe('Wrong address or password.')
    expect(el.querySelector<HTMLInputElement>('input[name="password"]')!.value).toBe('wrong')
    expect(auth.account).toBeNull()

    status = 429
    submit(el)
    await until(() => text(el.querySelector('[role="alert"]')).startsWith('Too many'), fixture)
    expect(text(el.querySelector('[role="alert"]'))).toBe('Too many failed logins. Try again in 2 minutes.')
  })

  it('asks for a code when the account has two-step login, or a recovery code instead', async () => {
    const calls = server({
      '/auth/login': { status: 200, body: { step: 'totp' } },
      '/auth/login/totp': (body) => ((body as { code?: string }).code === '123456' ? { status: 200, body: { username: 'dev@robinmail.test' } } : { status: 401, body: { error: 'wrong code' } }),
    })
    const { fixture, router, el } = await open()
    type(el.querySelector<HTMLInputElement>('input[name="username"]')!, 'dev@robinmail.test')
    type(el.querySelector<HTMLInputElement>('input[name="password"]')!, 'secret')
    await fixture.whenStable()
    submit(el)
    await until(() => !!el.querySelector('[data-testid="code-form"]'), fixture)

    type(el.querySelector<HTMLInputElement>('input[name="code"]')!, '000 000')
    await fixture.whenStable()
    submit(el)
    await until(() => !!el.querySelector('[role="alert"]'), fixture)
    expect(text(el.querySelector('[role="alert"]'))).toContain('That code is not right')

    el.querySelector<HTMLButtonElement>('[data-testid="toggle-recovery"]')!.click()
    await fixture.whenStable()
    expect(text(el.querySelector('label'))).toBe('Recovery code')
    el.querySelector<HTMLButtonElement>('[data-testid="toggle-recovery"]')!.click()
    await fixture.whenStable()

    type(el.querySelector<HTMLInputElement>('input[name="code"]')!, '123 456')
    await fixture.whenStable()
    submit(el)
    await until(() => router.url === '/mail')
    expect(calls.filter((c) => c.path === '/auth/login/totp').map((c) => c.body)).toEqual([{ code: '000000' }, { code: '123456' }])
  })
})
