import { provideZonelessChangeDetection } from '@angular/core'
import { TestBed } from '@angular/core/testing'
import type { Email } from '@robinmail/jmap'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { text, until } from '../testing/helpers'
import { MessageBody } from './message-body'

const base = {
  id: 'e1', blobId: 'b1', threadId: 't1', mailboxIds: { m1: true }, keywords: {}, size: 100, receivedAt: '2026-11-14T10:00:00Z',
  hasAttachment: false, preview: '', subject: 'Hello', from: [{ name: 'Ann', email: 'ann@example.org' }], to: [],
} as unknown as Email

function email(over: Partial<Email>): Email {
  return { ...base, ...over } as Email
}

async function render(e: Email) {
  TestBed.configureTestingModule({ providers: [provideZonelessChangeDetection()] })
  const fixture = TestBed.createComponent(MessageBody)
  fixture.componentRef.setInput('email', e)
  await fixture.whenStable()
  return { fixture, el: fixture.nativeElement as HTMLElement }
}

describe('the body of a message', () => {
  beforeEach(() => {
    // What the page asks the server about remote images.
    vi.stubGlobal('fetch', vi.fn(async () => new Response('', { status: 200, headers: { 'X-Remote-Images': 'blocked' } })))
  })
  afterEach(() => vi.unstubAllGlobals())

  it('shows text as text, with links made by the page', async () => {
    const { el } = await render(email({
      textBody: [{ partId: '1', type: 'text/plain' }] as Email['textBody'],
      bodyValues: { '1': { value: 'See https://example.org/a. <script>alert(1)</script> or write ann@example.org', isTruncated: false, isEncodingProblem: false } },
    }))
    const pre = el.querySelector('[data-testid="text-view"]')!
    expect(text(pre)).toBe('See https://example.org/a. <script>alert(1)</script> or write ann@example.org')
    expect(pre.querySelector('script')).toBeNull()
    expect([...pre.querySelectorAll('a')].map((a) => [a.getAttribute('href'), a.getAttribute('target'), a.getAttribute('rel')])).toEqual([
      ['https://example.org/a', '_blank', 'noopener noreferrer'],
      ['mailto:ann@example.org', null, 'noopener noreferrer'],
    ])
    expect(el.querySelector('iframe')).toBeNull()
  })

  it('shows HTML only as the server\'s view in a frame without scripts or an origin', async () => {
    const { el } = await render(email({
      htmlBody: [{ partId: '2', type: 'text/html' }] as Email['htmlBody'],
      textBody: [{ partId: '2', type: 'text/html' }] as Email['textBody'],
      bodyValues: { '2': { value: '<h1 id="from-the-message">Five things</h1><script>alert(1)</script>', isTruncated: false, isEncodingProblem: false } },
    }))
    const frame = el.querySelector('iframe')!
    expect(frame.getAttribute('sandbox')).toBe('allow-popups allow-popups-to-escape-sandbox')
    expect(frame.getAttribute('referrerpolicy')).toBe('no-referrer')
    expect(frame.getAttribute('src')).toMatch(/\/jmap\/view\/.*e1/)
    // Nothing of the message's markup is in this page.
    expect(el.querySelector('#from-the-message')).toBeNull()
    expect(el.querySelector('script')).toBeNull()
    expect(el.textContent).not.toContain('Five things')
  })

  it('lists attachments as downloads', async () => {
    const { el } = await render(email({
      textBody: [], bodyValues: {},
      attachments: [{ partId: '3', blobId: 'blob3', size: 2048, name: 'notes.txt', type: 'text/plain', charset: null, disposition: 'attachment', cid: null }] as Email['attachments'],
    }))
    const a = el.querySelector<HTMLAnchorElement>('[data-testid="attachment"]')!
    expect(text(a)).toBe('notes.txt2.0 kB')
    expect(a.getAttribute('download')).toBe('notes.txt')
    expect(a.getAttribute('href')).toContain('blob3')
    expect(text(el.querySelector('[data-testid="text-view"]'))).toBe('(This message has no text.)')
  })

  it('offers to load remote images when the server left some out', async () => {
    const { fixture, el } = await render(email({ htmlBody: [{ partId: '2', type: 'text/html' }] as Email['htmlBody'], bodyValues: {} }))
    await until(() => !!el.querySelector('[data-testid="load-images"]'), fixture)
    el.querySelector<HTMLButtonElement>('[data-testid="load-images"]')!.click()
    await fixture.whenStable()
    expect(el.querySelector('iframe')!.getAttribute('src')).toMatch(/images=1$/)
    expect(el.querySelector('[data-testid="load-images"]')).toBeNull()
  })
})
