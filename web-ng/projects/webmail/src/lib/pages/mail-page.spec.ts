// The mail page against the fake JMAP server of the client's own tests:
// the list, opening a thread, marking, deleting, and the keys.

import { provideZonelessChangeDetection } from '@angular/core'
import { type ComponentFixture, TestBed } from '@angular/core/testing'
import { Router, provideRouter } from '@angular/router'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { FakeServer } from '../../../../jmap/src/lib/test/fake'
import { auth, compose, store } from '../app'
import { Frame } from '../frame'
import { provideWebmail, webmailRoutes } from '../routes'
import { text, until } from '../testing/helpers'

let server: FakeServer
let fixture: ComponentFixture<Frame>
let router: Router
let el: HTMLElement

const rows = () => [...el.querySelectorAll<HTMLElement>('[data-row]')]
const subjects = () => rows().map((r) => text(r.querySelector('a > div:nth-child(2) > span.truncate')))
const key = (k: string) => window.dispatchEvent(new KeyboardEvent('keydown', { key: k, bubbles: true, cancelable: true }))

async function open(url = '/mail') {
  TestBed.configureTestingModule({ providers: [provideZonelessChangeDetection(), provideRouter(webmailRoutes), provideWebmail()] })
  router = TestBed.inject(Router)
  fixture = TestBed.createComponent(Frame)
  el = fixture.nativeElement
  await router.navigateByUrl(url)
  await until(() => store.state.ready && !!store.state.list.spec && !store.state.list.loading, fixture)
  fixture.detectChanges()
}

describe('the mail page', () => {
  beforeEach(() => {
    server = new FakeServer()
    vi.stubGlobal('fetch', server.fetch)
    // No event source in this DOM: the page falls back to its timer.
    vi.stubGlobal('EventSource', class {
      onopen = null
      onerror = null
      addEventListener() {}
      close() {}
    })
    auth.checked = true
    auth.account = { username: 'dev@robinmail.test' } as typeof auth.account
  })
  afterEach(() => {
    fixture?.destroy()
    compose.clear()
    store.reset()
    vi.unstubAllGlobals()
  })

  it('goes to the inbox and lists its threads, newest first, with the unread count in the sidebar', async () => {
    const lunch = server.deliver({ subject: 'Lunch', from: 'anna@example.org' })
    server.deliver({ subject: 'Report', seen: true })
    server.deliver({ subject: 'Re: Lunch', thread: lunch.threadId, from: 'bert@example.org' })
    await open()
    expect(router.url).toBe('/mail/m1')
    expect(subjects()).toEqual(['Re: Lunch', 'Report'])
    expect(rows().map((r) => r.hasAttribute('data-unread'))).toEqual([true, false])
    expect(text(rows()[0]!.querySelector('a'))).toContain('anna@example.org, bert@example.org 2')
    expect(text(el.querySelector('a[data-mailbox="inbox"] [data-unread]'))).toBe('2')
    expect(document.title).toBe('(2) Inbox · robinmail')
  })

  it('opens a thread from its row, shows the text and marks it read on the server', async () => {
    const e = server.deliver({ subject: 'Lunch' })
    await open()
    rows()[0]!.querySelector('a')!.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, button: 0 }))
    await until(() => !!el.querySelector('[data-testid="text-view"]'), fixture)
    expect(router.url).toBe(`/mail/m1/${e.threadId}`)
    expect(text(el.querySelector('[data-testid="thread-subject"]'))).toBe('Lunch')
    await until(() => !!server.emails.get(e.id)!.keywords.$seen)
    await until(() => !rows()[0]!.hasAttribute('data-unread'), fixture)
    expect(el.querySelector('a[data-mailbox="inbox"] [data-unread]')).toBeNull()
  })

  it('survives a reload of an address with a thread and a filter', async () => {
    const e = server.deliver({ subject: 'Flagged one' })
    server.touch(e.id, (x) => (x.keywords.$flagged = true))
    server.deliver({ subject: 'Other' })
    await open(`/mail/m1/${e.threadId}?flagged=1`)
    await until(() => !!el.querySelector('[data-testid="thread-subject"]'), fixture)
    expect(subjects()).toEqual(['Flagged one'])
    expect(text(el.querySelector('[data-testid="thread-subject"]'))).toBe('Flagged one')
    expect(text(el.querySelector('button[aria-label="Remove filter Flagged"]')?.parentElement)).toBe('Flagged')
  })

  it('flags from the row, and moves to Trash with the key', async () => {
    const e = server.deliver({ subject: 'Lunch', seen: true })
    await open()
    rows()[0]!.querySelector<HTMLButtonElement>('[data-testid="row-flag"]')!.click()
    await until(() => !!server.emails.get(e.id)!.keywords.$flagged)
    await until(() => rows()[0]!.querySelector('[data-testid="row-flag"]')!.getAttribute('aria-pressed') === 'true', fixture)

    rows()[0]!.querySelector<HTMLInputElement>('input[type="checkbox"]')!.click()
    await until(() => text(el.querySelector('[data-testid="selected-count"]')) === '1 selected', fixture)
    key('#')
    await until(() => rows().length === 0, fixture)
    const trash = [...server.mailboxes.values()].find((m) => m.role === 'trash')!
    await until(() => !!server.emails.get(e.id)!.mailboxIds[trash.id])
    expect(server.emails.get(e.id)!.mailboxIds).toEqual({ [trash.id]: true })
    expect(text(el.querySelector('[data-testid="message-list"]'))).toContain('No messages here.')
  })

  it('steps through the list with j and k and leaves the thread with Escape', async () => {
    const a = server.deliver({ subject: 'First', seen: true })
    const b = server.deliver({ subject: 'Second', seen: true })
    await open()
    key('j')
    await until(() => router.url === `/mail/m1/${b.threadId}`)
    key('j')
    await until(() => router.url === `/mail/m1/${a.threadId}`)
    key('k')
    await until(() => router.url === `/mail/m1/${b.threadId}`)
    key('Escape')
    await until(() => router.url === '/mail/m1')
  })

  it('opens a composer with c, and a draft is written to the server', async () => {
    await open()
    await until(() => compose.state.identitiesLoaded)
    key('c')
    await until(() => !!el.querySelector('[data-testid="composer"]'), fixture)
    const subject = el.querySelector<HTMLInputElement>('[data-testid="composer"] input[id$="-subject"]')!
    subject.value = 'A draft'
    subject.dispatchEvent(new Event('input', { bubbles: true }))
    await until(() => [...server.emails.values()].some((e) => e.subject === 'A draft' && e.keywords.$draft), fixture, 5000)
    await until(() => text(el.querySelector('[data-testid="composer-status"]')) === 'Saved', fixture)
  })
})
