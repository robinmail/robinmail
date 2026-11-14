// A small JMAP client: the session resource, batched method calls with
// result references, and the state string of every data type. It holds no
// credentials: the browser sends the session cookie by itself.

import type { Id, Invocation, MethodName, Methods, ResultReference, Session } from './types'

export const USING = ['urn:ietf:params:jmap:core', 'urn:ietf:params:jmap:mail', 'urn:ietf:params:jmap:submission']

// HttpError is a response that is not JMAP: 401 without a session, 5xx.
export class HttpError extends Error {
  constructor(public status: number, message: string) {
    super(message)
    this.name = 'HttpError'
  }
}

// MethodError is a method-level error ("error" invocation, RFC 8620 3.6.2)
// or a request-level problem document (3.6.1).
export class MethodError extends Error {
  constructor(public type: string, public description: string, public method = '') {
    super(description ? `${type}: ${description}` : type)
    this.name = 'MethodError'
  }
}

export function isMethodError(e: unknown, type?: string): e is MethodError {
  return e instanceof MethodError && (type === undefined || e.type === type)
}

// Arguments of a call: the method's own, each of which may instead be given
// as "#name" with a result reference.
type Args<M extends MethodName> = Methods[M]['args'] & { [ref: `#${string}`]: ResultReference }

// A Call is one method call of a batch. ref() points a later call of the
// same batch at part of its result.
export class Call<M extends MethodName> {
  constructor(public readonly name: M, public readonly id: string) {}
  ref(path: string): ResultReference {
    return { resultOf: this.id, name: this.name, path }
  }
}

export class Results {
  private byId = new Map<string, Invocation>()
  constructor(responses: Invocation[]) {
    // A method may answer more than once (RFC 8620 3.4); the webmail calls
    // none that does, so the first response of a call id is its answer.
    for (const r of responses) {
      if (!this.byId.has(r[2])) this.byId.set(r[2], r)
    }
  }

  // get returns the response of a call, or throws its MethodError.
  get<M extends MethodName>(call: Call<M>): Methods[M]['response'] {
    const r = this.byId.get(call.id)
    if (!r) throw new MethodError('serverFail', 'the server sent no response for this call', call.name)
    if (r[0] === 'error') {
      const a = r[1] as { type?: string; description?: string }
      throw new MethodError(a.type ?? 'serverFail', a.description ?? '', call.name)
    }
    return r[1] as Methods[M]['response']
  }

  // error returns the MethodError of a call, if it failed.
  error(call: Call<MethodName>): MethodError | null {
    try {
      this.get(call)
      return null
    } catch (e) {
      return e as MethodError
    }
  }
}

export class Batch {
  private calls: Invocation[] = []
  constructor(private client: JmapClient) {}

  // call adds a method call. accountId is filled in.
  call<M extends MethodName>(name: M, args: Args<M>): Call<M> {
    const id = `c${this.calls.length}`
    this.calls.push([name, { ...args }, id])
    return new Call(name, id)
  }

  get size(): number {
    return this.calls.length
  }

  async send(): Promise<Results> {
    // The account is known once the session is: a first batch loads it.
    if (!this.client.session) await this.client.loadSession()
    const accountId = this.client.accountId
    const calls = this.calls.map(([name, args, id]): Invocation => [name, name === 'Core/echo' ? args : { accountId, ...args }, id])
    return new Results(await this.client.request(calls))
  }
}

export interface Upload {
  blobId: Id
  type: string
  size: number
}

export interface UploadOptions {
  // Told how far the upload is, from 0 to 1.
  onProgress?: (fraction: number) => void
  signal?: AbortSignal
}

// Uploader sends a file to a path and resolves to the HTTP status and the
// body of the answer. The browser's one is XMLHttpRequest, the only way a
// page learns how far an upload is.
export type Uploader = (path: string, file: Blob, type: string, opts: UploadOptions) => Promise<{ status: number; body: string }>

export const xhrUploader: Uploader = (path, file, type, opts) =>
  new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open('POST', path)
    xhr.withCredentials = true
    xhr.setRequestHeader('Content-Type', type)
    xhr.setRequestHeader('Accept', 'application/json')
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable && e.total > 0) opts.onProgress?.(e.loaded / e.total)
    }
    xhr.onload = () => resolve({ status: xhr.status, body: xhr.responseText })
    xhr.onerror = () => reject(new Error('The connection was lost during the upload.'))
    xhr.onabort = () => reject(new DOMException('The upload was cancelled.', 'AbortError'))
    if (opts.signal?.aborted) {
      reject(new DOMException('The upload was cancelled.', 'AbortError'))
      return
    }
    opts.signal?.addEventListener('abort', () => xhr.abort())
    xhr.send(file)
  })

export interface ClientOptions {
  fetch?: typeof fetch
  uploader?: Uploader
  // Called when the server answers 401: the session cookie is gone or expired.
  onUnauthorized?: () => void
}

// localPath drops scheme and host from a URL of the session resource. The
// webmail is always served from the origin of the API (in development Vite
// proxies it), so every request is same-origin and carries the cookie.
export function localPath(url: string): string {
  return url.replace(/^[a-z][a-z0-9+.-]*:\/\/[^/]+/i, '')
}

export class JmapClient {
  session: Session | null = null
  accountId: Id = ''
  // The last known state string per data type ("Email", "Mailbox", "Thread").
  readonly states: Record<string, string> = {}
  private fetch: typeof fetch
  private uploader: Uploader
  // Called when the server answers 401: the session is gone or expired.
  onUnauthorized: () => void

  constructor(opts: ClientOptions = {}) {
    this.fetch = opts.fetch ?? ((input, init) => fetch(input, init))
    this.uploader = opts.uploader ?? xhrUploader
    this.onUnauthorized = opts.onUnauthorized ?? (() => {})
  }

  private async http(path: string, init: RequestInit): Promise<Response> {
    const res = await this.fetch(path, { credentials: 'same-origin', ...init })
    if (res.status === 401) {
      this.reset()
      this.onUnauthorized()
      throw new HttpError(401, 'not logged in')
    }
    return res
  }

  reset(): void {
    this.session = null
    this.accountId = ''
    for (const k of Object.keys(this.states)) delete this.states[k]
  }

  async loadSession(): Promise<Session> {
    const res = await this.http('/.well-known/jmap', { headers: { Accept: 'application/json' } })
    if (!res.ok) throw new HttpError(res.status, `session: HTTP ${res.status}`)
    const session = (await res.json()) as Session
    this.session = session
    this.accountId = session.primaryAccounts['urn:ietf:params:jmap:mail'] ?? Object.keys(session.accounts)[0] ?? ''
    return session
  }

  batch(): Batch {
    return new Batch(this)
  }

  // request sends method calls and returns the method responses. It notes
  // the state strings the responses carry.
  async request(methodCalls: Invocation[]): Promise<Invocation[]> {
    if (!this.session) await this.loadSession()
    const body = JSON.stringify({ using: USING, methodCalls })
    const res = await this.http(localPath(this.session!.apiUrl), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
      body,
      // A change must reach the server also when the tab is closed right
      // after the click: the page showed it as done already. (Browsers
      // allow keepalive for small bodies only.)
      keepalive: body.length < 60_000 && methodCalls.some((c) => c[0].endsWith('/set')),
    })
    if (!res.ok) {
      // A request-level error is a problem document with a type URN.
      let type = 'serverFail'
      let detail = `HTTP ${res.status}`
      try {
        const p = (await res.json()) as { type?: string; detail?: string }
        if (p.type) type = p.type.replace('urn:ietf:params:jmap:error:', '')
        if (p.detail) detail = p.detail
      } catch {
        throw new HttpError(res.status, detail)
      }
      throw new MethodError(type, detail)
    }
    const answer = (await res.json()) as { methodResponses: Invocation[]; sessionState?: string }
    for (const r of answer.methodResponses) this.noteState(r)
    return answer.methodResponses
  }

  // noteState moves the state of a type on after a /set of ours, but only
  // when the /set started from the state we hold: then nothing happened in
  // between and its own changes need not be fetched again. Otherwise the
  // state stays, and the next /changes reports what we missed along with
  // what we did. The state of a /get or /changes is not taken here: it only
  // counts once the caller has merged the data (see setState).
  //
  // A /set that created something does not move the state either: what it
  // made is not in the store yet, and /changes is how it gets there.
  private noteState([name, args]: Invocation): void {
    const [type, method] = name.split('/') as [string, string | undefined]
    if (method !== 'set' || typeof args.newState !== 'string') return
    if (args.created && Object.keys(args.created).length > 0) return
    if (args.oldState === this.states[type]) this.states[type] = args.newState
  }

  setState(type: string, state: string): void {
    this.states[type] = state
  }

  // maxSizeUpload is the largest file the server takes (RFC 8620 2).
  get maxSizeUpload(): number {
    const core = this.session?.capabilities['urn:ietf:params:jmap:core'] as { maxSizeUpload?: number } | undefined
    return core?.maxSizeUpload ?? 0
  }

  // upload stores a file as a blob (RFC 8620 6.1) and answers its id.
  async upload(file: Blob, type: string, opts: UploadOptions = {}): Promise<Upload> {
    if (!this.session) await this.loadSession()
    const path = localPath(this.session!.uploadUrl).replace('{accountId}', encodeURIComponent(this.accountId))
    const res = await this.uploader(path, file, type || 'application/octet-stream', opts)
    if (res.status === 401) {
      this.reset()
      this.onUnauthorized()
      throw new HttpError(401, 'not logged in')
    }
    if (res.status < 200 || res.status > 299) {
      let detail = `HTTP ${res.status}`
      try {
        const p = JSON.parse(res.body) as { detail?: string }
        if (p.detail) detail = p.detail
      } catch {
        // not a problem document: keep the status
      }
      throw new HttpError(res.status, res.status === 413 ? 'The file is larger than the server accepts.' : detail)
    }
    return JSON.parse(res.body) as Upload
  }

  // downloadUrl fills the session's download template (RFC 8620 6.2).
  downloadUrl(blobId: Id, name: string, type: string): string {
    const template = localPath(this.session?.downloadUrl ?? '/jmap/download/{accountId}/{blobId}/{name}?type={type}')
    return template
      .replace('{accountId}', encodeURIComponent(this.accountId))
      .replace('{blobId}', encodeURIComponent(blobId))
      .replace('{name}', encodeURIComponent(name || 'download'))
      .replace('{type}', encodeURIComponent(type || 'application/octet-stream'))
  }

  // viewUrl is robinmail's sanitised HTML view of a message, for a
  // sandboxed iframe. Not part of JMAP.
  viewUrl(emailId: Id, images: boolean): string {
    return `/jmap/view/${encodeURIComponent(this.accountId)}/${encodeURIComponent(emailId)}${images ? '?images=1' : ''}`
  }

  // remoteImages asks whether the view left out remote images.
  async remoteImages(emailId: Id): Promise<'blocked' | 'shown' | 'none'> {
    const res = await this.http(this.viewUrl(emailId, false), { method: 'HEAD' })
    const v = res.headers.get('X-Remote-Images')
    return v === 'blocked' || v === 'shown' ? v : 'none'
  }
}

// Login endpoints of robinmail (not JMAP).

export interface Account {
  accountId: Id
  username: string
}

export class LoginError extends Error {
  constructor(public status: number, message: string, public retryAfter = 0) {
    super(message)
    this.name = 'LoginError'
  }
}

export async function login(username: string, password: string, f: typeof fetch = fetch): Promise<Account> {
  const res = await f('/auth/login', {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password }),
  })
  if (!res.ok) {
    let message = `HTTP ${res.status}`
    try {
      message = ((await res.json()) as { error?: string }).error ?? message
    } catch {
      // not JSON: keep the status
    }
    throw new LoginError(res.status, message, Number(res.headers.get('Retry-After') ?? 0))
  }
  return (await res.json()) as Account
}

export async function logout(f: typeof fetch = fetch): Promise<void> {
  await f('/auth/logout', { method: 'POST', credentials: 'same-origin' })
}

// currentAccount answers who the cookie belongs to, or null.
export async function currentAccount(f: typeof fetch = fetch): Promise<Account | null> {
  const res = await f('/auth/session', { credentials: 'same-origin' })
  if (res.status === 401) return null
  if (!res.ok) throw new HttpError(res.status, `session: HTTP ${res.status}`)
  return (await res.json()) as Account
}
