// The endpoints under /auth that are not JMAP: the login with its second
// step, and what a logged-in user changes about the ways into the account
// (password, app passwords, two-step login, sessions).
//
// Every request that changes something is JSON with its content type, also
// when it has nothing to say: the server refuses anything else, so that a
// form on another site cannot send one.

export interface Account {
  accountId: string
  username: string
}

// Session is what GET /auth/session says about the login behind the cookie.
export interface Session extends Account {
  sessionId: string
  // How the user got in: "password", "password+totp", "oidc:<provider>".
  method: string
  totp: boolean
  // false for an account that only logs in through a provider.
  hasPassword: boolean
  appPasswords: number
  // Whether the account password still works in mail programs.
  passwordOpensMail: boolean
}

export interface Provider {
  name: string
  label: string
}

export interface AppPassword {
  id: string
  name: string
  // The protocols it opens; none is all.
  scopes: string[]
  createdAt: string
  usedAt: string | null
  usedIp?: string
  // Only in the answer that creates it.
  secret?: string
}

export interface WebSession {
  id: string
  current: boolean
  device: string
  agent: string
  method: string
  ip: string
  seenIp: string
  createdAt: string
  seenAt: string
  expiresAt: string
  remember: boolean
}

export interface Enrolment {
  secret: string
  uri: string
  // The QR code of the URI: one string of "0" and "1" per row. Empty when
  // the URI does not fit a code.
  qr: string[] | null
}

// AuthError is an answer that is not a success. reason is "password" for a
// wrong account password and "stale" for a login that is too old for the
// change; step is "totp" when a login waits for its code.
export class AuthError extends Error {
  constructor(
    public status: number,
    message: string,
    public retryAfter = 0,
    public reason = '',
    public step = '',
  ) {
    super(message)
    this.name = 'AuthError'
  }
}

type Fetch = typeof fetch

async function request<T>(f: Fetch, method: string, path: string, body?: unknown): Promise<T> {
  const init: RequestInit = { method, credentials: 'same-origin' }
  if (method !== 'GET') {
    init.headers = { 'Content-Type': 'application/json' }
    init.body = JSON.stringify(body ?? {})
  }
  const res = await f(path, init)
  if (!res.ok) {
    let answer: { error?: string; reason?: string; step?: string } = {}
    try {
      answer = (await res.json()) as typeof answer
    } catch {
      // not JSON: the status has to do
    }
    throw new AuthError(res.status, answer.error ?? `HTTP ${res.status}`, Number(res.headers.get('Retry-After') ?? 0), answer.reason ?? '', answer.step ?? '')
  }
  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

// login is the first step. It answers with the account, or with
// { step: 'totp' } when the account has two-step login: then loginCode
// finishes it.
export function login(username: string, password: string, remember = false, f: Fetch = fetch): Promise<Account | { step: 'totp' }> {
  return request(f, 'POST', '/auth/login', { username, password, remember })
}

// loginCode is the second step: a code of the authenticator, or a recovery code.
export function loginCode(code: { code: string } | { recoveryCode: string }, f: Fetch = fetch): Promise<Account> {
  return request(f, 'POST', '/auth/login/totp', code)
}

export function logout(f: Fetch = fetch): Promise<void> {
  return request(f, 'POST', '/auth/logout')
}

// currentSession answers who the cookie belongs to, or null. waiting is
// true when a login in this browser still owes its code.
export async function currentSession(f: Fetch = fetch): Promise<{ session: Session | null; waiting: boolean }> {
  try {
    return { session: await request<Session>(f, 'GET', '/auth/session'), waiting: false }
  } catch (e) {
    if (e instanceof AuthError && e.status === 401) return { session: null, waiting: e.step === 'totp' }
    throw e
  }
}

export async function providers(f: Fetch = fetch): Promise<Provider[]> {
  return (await request<{ providers: Provider[] }>(f, 'GET', '/auth/config')).providers
}

export function changePassword(current: string, next: string, f: Fetch = fetch): Promise<void> {
  return request(f, 'POST', '/auth/password', { current, new: next })
}

export function appPasswords(f: Fetch = fetch): Promise<{ list: AppPassword[]; passwordOpensMail: boolean }> {
  return request(f, 'GET', '/auth/app-passwords')
}

export function createAppPassword(name: string, scopes: string[], password: string, f: Fetch = fetch): Promise<AppPassword> {
  return request(f, 'POST', '/auth/app-passwords', { name, scopes, password })
}

export function revokeAppPassword(id: string, f: Fetch = fetch): Promise<void> {
  return request(f, 'DELETE', `/auth/app-passwords/${encodeURIComponent(id)}`)
}

export function totpState(f: Fetch = fetch): Promise<{ enabled: boolean; recoveryCodes: number }> {
  return request(f, 'GET', '/auth/totp')
}

export function totpEnrol(password: string, f: Fetch = fetch): Promise<Enrolment> {
  return request(f, 'POST', '/auth/totp/enrol', { password })
}

export async function totpConfirm(code: string, f: Fetch = fetch): Promise<string[]> {
  return (await request<{ recoveryCodes: string[] }>(f, 'POST', '/auth/totp/confirm', { code })).recoveryCodes
}

// secondProof is a code or a recovery code, whichever the user typed: a
// code is six digits, anything else is tried as a recovery code.
export function secondProof(typed: string): { code: string } | { recoveryCode: string } {
  const t = typed.trim()
  return /^\d{6}$/.test(t.replace(/\s/g, '')) ? { code: t.replace(/\s/g, '') } : { recoveryCode: t }
}

export function totpDisable(password: string, typed: string, f: Fetch = fetch): Promise<void> {
  return request(f, 'POST', '/auth/totp/disable', { password, ...secondProof(typed) })
}

export async function newRecoveryCodes(password: string, typed: string, f: Fetch = fetch): Promise<string[]> {
  return (await request<{ recoveryCodes: string[] }>(f, 'POST', '/auth/totp/recovery-codes', { password, ...secondProof(typed) })).recoveryCodes
}

export async function sessions(f: Fetch = fetch): Promise<WebSession[]> {
  return (await request<{ list: WebSession[] }>(f, 'GET', '/auth/sessions')).list
}

export function revokeSession(id: string, f: Fetch = fetch): Promise<void> {
  return request(f, 'DELETE', `/auth/sessions/${encodeURIComponent(id)}`)
}

export async function revokeOtherSessions(f: Fetch = fetch): Promise<number> {
  return (await request<{ revoked: number }>(f, 'POST', '/auth/sessions/revoke-others')).revoked
}

// qrPath turns the rows of a QR code into the path of an SVG: a square of
// one unit for each dark module, runs of them joined. The caller draws it
// in a viewBox with the quiet zone of four modules around it.
export function qrPath(rows: string[]): string {
  let d = ''
  rows.forEach((row, y) => {
    for (let x = 0; x < row.length; x++) {
      if (row[x] !== '1') continue
      let w = 1
      while (row[x + w] === '1') w++
      d += `M${x} ${y}h${w}v1h-${w}z`
      x += w
    }
  })
  return d
}

// explain puts an error of this API into words for the user.
export function explain(e: unknown): string {
  if (e instanceof AuthError) {
    if (e.status === 429) {
      const minutes = Math.max(1, Math.ceil(e.retryAfter / 60))
      return `Too many wrong tries. Try again in ${minutes} ${minutes === 1 ? 'minute' : 'minutes'}.`
    }
    if (e.reason === 'password') return 'That is not your password.'
    if (e.reason === 'stale') return 'Log out and in again to change this: your login is more than a few minutes old.'
    if (e.status === 401 && e.message === 'wrong code') return 'That code is not right. Codes change every 30 seconds and work once.'
    if (e.status === 401) return 'You are no longer logged in.'
    if (e.status >= 500) return `The server cannot do that right now (${e.status}).`
    return e.message.charAt(0).toUpperCase() + e.message.slice(1) + '.'
  }
  return 'The server does not answer. Check the connection and try again.'
}

// addresses lists what the user may send as: their own address first, then
// the aliases that deliver to them.
export async function addresses(f: Fetch = fetch): Promise<string[]> {
  return (await request<{ addresses: string[] }>(f, 'GET', '/auth/addresses')).addresses
}

// MailServer is one way a mail program reaches the server.
export interface MailServer {
  host: string
  port: number
  // "TLS" from the first byte, or "STARTTLS".
  security: string
}

// MailSettings is what a user types into a mail program that does not
// find the settings itself.
export interface MailSettings {
  username: string
  imap: MailServer[]
  smtp: MailServer[]
  // Whether /auth/mobileconfig has a profile for Apple devices.
  profile: boolean
}

export function mailSettings(f: Fetch = fetch): Promise<MailSettings> {
  return request(f, 'GET', '/auth/mail-settings')
}
