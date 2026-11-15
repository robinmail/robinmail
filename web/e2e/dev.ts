// Helpers of the end-to-end tests: the development instance of
// contrib/dev/seed.sh, reached through its command line and its login.

import { execFileSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { expect, type Page } from '@playwright/test'

const root = fileURLToPath(new URL('../..', import.meta.url))

export const USER = 'dev@robinmail.test'
// The second user of the seed script: someone to send mail to.
export const OTHER = 'other@robinmail.test'
export const PASSWORD = process.env.ROBINMAIL_DEV_PASSWORD ?? 'robinmail-dev'
// The config of the instance under test: e2e.sh runs one of its own, with
// a data directory of its own, and says which.
export const CONFIG = process.env.ROBINMAIL_E2E_CONFIG ?? 'dev/robinmail.json'

// deliver stores a message in the dev account, as the seed script does.
export function deliver(message: string, mailbox = 'inbox', user = USER): void {
  execFileSync('dev/robinmail', ['-config', CONFIG, 'deliver', user, mailbox], {
    cwd: root,
    input: message.replace(/\r?\n/g, '\r\n'),
  })
}

export function cli(...args: string[]): string {
  return execFileSync('dev/robinmail', ['-config', CONFIG, ...args], { cwd: root, encoding: 'utf8' })
}

export function textMessage(subject: string, n: number): string {
  return `From: Sender ${n} <sender${n}@example.org>
To: ${USER}
Subject: ${subject}
Date: ${new Date(Date.now() - n * 60_000).toUTCString().replace('GMT', '+0000')}
Message-ID: <${subject.replace(/[^a-z0-9]/gi, '')}@example.org>

Message number ${n}.
`
}

// htmlMessage is a message with an HTML body, a remote image and an
// attachment. The subject makes it unique, so a rerun delivers a new one.
export function htmlMessage(subject: string): string {
  return `From: "Weekly Digest" <news@lists.example>
To: ${USER}
Subject: ${subject}
Date: ${new Date().toUTCString().replace('GMT', '+0000')}
Message-ID: <${subject.replace(/[^a-z0-9]/gi, '')}@lists.example>
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="mix"

--mix
Content-Type: text/html; charset=utf-8

<html><body>
<img src="https://lists.example/header.png" width="600" alt="Weekly Digest">
<h1>Five things worth reading</h1>
<p>Body of ${subject}. <a href="https://example.org/one">A link</a></p>
<script>document.title = "tracked"</script>
</body></html>
--mix
Content-Type: text/plain; name="notes.txt"
Content-Disposition: attachment; filename="notes.txt"

These are the notes.
--mix--
`
}

export async function login(page: Page, user = USER): Promise<void> {
  await page.goto('/')
  await expect(page).toHaveURL(/\/login/)
  await page.getByLabel('Email address').fill(user)
  await page.getByLabel('Password').fill(PASSWORD)
  await page.getByRole('button', { name: 'Log in' }).click()
  await expect(page).toHaveURL(/\/mail\/m\d+$/)
}

// destroyBySubject removes the messages a test delivered, through JMAP
// with the page's session, and marks the given emails unread again.
export async function destroyBySubject(page: Page, subject: string, unread: string[] = []): Promise<void> {
  const session = await (await page.request.get('/.well-known/jmap')).json()
  const accountId = session.primaryAccounts['urn:ietf:params:jmap:mail']
  await page.request.post('/jmap/api', {
    data: {
      using: ['urn:ietf:params:jmap:core', 'urn:ietf:params:jmap:mail'],
      methodCalls: [
        ['Email/query', { accountId, filter: { subject } }, 'q'],
        ['Email/set', { accountId, '#destroy': { resultOf: 'q', name: 'Email/query', path: '/ids' } }, 'd'],
        ['Email/query', { accountId, filter: { operator: 'OR', conditions: unread.map((s) => ({ subject: s })) } }, 'u'],
        ['Email/get', { accountId, '#ids': { resultOf: 'u', name: 'Email/query', path: '/ids' }, properties: ['id'] }, 'g'],
      ],
    },
  }).then(async (r) => {
    const ids: string[] = (await r.json()).methodResponses[3][1].list?.map((e: { id: string }) => e.id) ?? []
    if (ids.length === 0) return
    await page.request.post('/jmap/api', {
      data: {
        using: ['urn:ietf:params:jmap:core', 'urn:ietf:params:jmap:mail'],
        methodCalls: [['Email/set', { accountId, update: Object.fromEntries(ids.map((id) => [id, { 'keywords/$seen': null }])) }, 's']],
      },
    })
  })
}

const USING = ['urn:ietf:params:jmap:core', 'urn:ietf:params:jmap:mail', 'urn:ietf:params:jmap:submission', 'urn:ietf:params:jmap:sieve', 'urn:ietf:params:jmap:vacationresponse',
  'urn:ietf:params:jmap:calendars', 'urn:ietf:params:jmap:contacts', 'urn:ietf:params:jmap:principals']

// angular says the server under test has the Angular webmail, which has
// the calendar and the contacts ("make e2e-vue" sets ROBINMAIL_E2E_WEBMAIL=vue).
export const angular = process.env.ROBINMAIL_E2E_WEBMAIL !== 'vue'

// jmap sends method calls with the page's session and returns the
// arguments of each response. accountId is filled in.
export async function jmap(page: Page, calls: [string, Record<string, unknown>][]): Promise<Record<string, any>[]> {
  const session = await (await page.request.get('/.well-known/jmap')).json()
  const accountId = session.primaryAccounts['urn:ietf:params:jmap:mail']
  const res = await page.request.post('/jmap/api', {
    data: { using: USING, methodCalls: calls.map(([name, args], i) => [name, { accountId, ...args }, `c${i}`]) },
  })
  return ((await res.json()).methodResponses as [string, Record<string, any>, string][]).map((r) => r[1])
}

// emailsBySubject lists the emails of the page's account whose subject contains the text.
export async function emailsBySubject(page: Page, subject: string, properties = ['id', 'subject', 'keywords', 'mailboxIds', 'threadId']): Promise<Record<string, any>[]> {
  const [, got] = await jmap(page, [
    ['Email/query', { filter: { subject } }],
    ['Email/get', { '#ids': { resultOf: 'c0', name: 'Email/query', path: '/ids' }, properties }],
  ])
  return got!.list
}

// cleanUp destroys the threads that have an email with the subject (a
// bounce is in the thread of the message it is about, under another
// subject), and the submissions of the account.
export async function cleanUp(page: Page, subject: string): Promise<void> {
  await jmap(page, [
    ['Email/query', { filter: { subject } }],
    ['Email/get', { '#ids': { resultOf: 'c0', name: 'Email/query', path: '/ids' }, properties: ['threadId'] }],
    ['Thread/get', { '#ids': { resultOf: 'c1', name: 'Email/get', path: '/list/*/threadId' } }],
    ['Email/set', { '#destroy': { resultOf: 'c2', name: 'Thread/get', path: '/list/*/emailIds' } }],
    ['EmailSubmission/query', {}],
    ['EmailSubmission/set', { '#destroy': { resultOf: 'c4', name: 'EmailSubmission/query', path: '/ids' } }],
  ])
}

// smtpDeliver hands a message to the tested server over SMTP, as another
// mail server would: unlike deliver, which stores it directly, this is the
// way on which aliases are followed and filters run. The address of the
// listener is read from the config e2e.sh wrote. It resolves to the last
// reply of the server (250 when the message was taken).
export async function smtpDeliver(from: string, to: string, message: string): Promise<string> {
  const { readFileSync } = await import('node:fs')
  const net = await import('node:net')
  const cfg = JSON.parse(readFileSync(new URL('../../dev/robinmail.e2e.json', import.meta.url), 'utf8'))
  const [host, port] = String(cfg.smtp.listen).split(':')
  const body = message.replace(/\r?\n/g, '\r\n').replace(/^\./gm, '..')
  const steps = ['EHLO e2e.test', `MAIL FROM:<${from}>`, `RCPT TO:<${to}>`, 'DATA', `${body.endsWith('\r\n') ? body : body + '\r\n'}.`, 'QUIT']
  return new Promise((resolve, reject) => {
    const socket = net.connect(Number(port), host || '127.0.0.1')
    let buffer = ''
    let last = ''
    let step = -1
    let refused = false
    socket.setEncoding('utf8')
    socket.setTimeout(15_000, () => socket.destroy(new Error('SMTP timed out')))
    socket.on('error', reject)
    socket.on('close', () => resolve(last))
    socket.on('data', (chunk: string) => {
      buffer += chunk
      // A reply is complete at a line whose fourth character is a space.
      for (;;) {
        const m = /^(?:\d{3}-[^\n]*\n)*(\d{3}) [^\n]*\n/.exec(buffer)
        if (!m) return
        buffer = buffer.slice(m[0].length)
        const code = Number(m[1])
        if (refused) return // the answer to our QUIT
        if (step >= 0 && steps[step] !== 'QUIT') last = m[0].trim().split('\n').pop()!.trim()
        if (code >= 400 && steps[step] !== 'QUIT') {
          refused = true
          socket.end('QUIT\r\n')
          return
        }
        step++
        if (step < steps.length) socket.write(steps[step] + '\r\n')
      }
    })
  })
}
