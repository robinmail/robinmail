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

// deliver stores a message in the dev account, as the seed script does.
export function deliver(message: string, mailbox = 'inbox', user = USER): void {
  execFileSync('dev/robinmail', ['-config', 'dev/robinmail.json', 'deliver', user, mailbox], {
    cwd: root,
    input: message.replace(/\r?\n/g, '\r\n'),
  })
}

export function cli(...args: string[]): string {
  return execFileSync('dev/robinmail', ['-config', 'dev/robinmail.json', ...args], { cwd: root, encoding: 'utf8' })
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

const USING = ['urn:ietf:params:jmap:core', 'urn:ietf:params:jmap:mail', 'urn:ietf:params:jmap:submission']

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
