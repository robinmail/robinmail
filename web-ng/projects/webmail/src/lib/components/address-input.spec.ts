import { Component, provideZonelessChangeDetection, signal } from '@angular/core'
import { TestBed } from '@angular/core/testing'
import type { EmailAddress } from '@robinmail/jmap'
import { describe, expect, it } from 'vitest'
import { text, type } from '../testing/helpers'
import { AddressInput } from './address-input'

@Component({
  imports: [AddressInput],
  template: `<rm-address-input [value]="to()" label="To" inputId="to" (valueChange)="to.set($event)" (invalid)="bad.set($event)" />`,
})
class Host {
  readonly to = signal<EmailAddress[]>([])
  readonly bad = signal(false)
}

async function setup() {
  TestBed.configureTestingModule({ providers: [provideZonelessChangeDetection()] })
  const fixture = TestBed.createComponent(Host)
  await fixture.whenStable()
  const el: HTMLElement = fixture.nativeElement
  const input = el.querySelector('input')!
  const key = (k: string) => input.dispatchEvent(new KeyboardEvent('keydown', { key: k, bubbles: true, cancelable: true }))
  const chips = () => [...el.querySelectorAll('[data-testid="address-chip"]')].map(text)
  return { fixture, el, input, key, chips, host: fixture.componentInstance }
}

describe('the address field', () => {
  it('turns what is typed into chips at Enter, a comma or leaving the field', async () => {
    const { fixture, input, key, chips, host } = await setup()
    type(input, 'ann@example.org')
    key('Enter')
    await fixture.whenStable()
    expect(chips()).toEqual(['ann@example.org'])
    expect(input.value).toBe('')

    type(input, 'Bert Jansen <bert@example.org>, carl@example.org')
    input.dispatchEvent(new Event('blur'))
    await fixture.whenStable()
    expect(host.to()).toEqual([{ name: null, email: 'ann@example.org' }, { name: 'Bert Jansen', email: 'bert@example.org' }, { name: null, email: 'carl@example.org' }])
    expect(chips()).toEqual(['ann@example.org', 'Bert Jansen', 'carl@example.org'])
  })

  it('keeps what is not an address in the field and says so', async () => {
    const { fixture, el, input, key, chips, host } = await setup()
    type(input, 'ann@example.org, not an address')
    key('Enter')
    await fixture.whenStable()
    expect(chips()).toEqual(['ann@example.org'])
    expect(input.value).toBe('not an address')
    expect(host.bad()).toBe(true)
    expect(input.getAttribute('aria-invalid')).toBe('true')
    expect(text(el.querySelector('[role="alert"]'))).toContain('not an email address')

    type(input, '')
    await fixture.whenStable()
    expect(host.bad()).toBe(false)
    expect(el.querySelector('[role="alert"]')).toBeNull()
  })

  it('takes the last chip away with Backspace in an empty field, and one by its button', async () => {
    const { fixture, el, key, chips, host } = await setup()
    host.to.set([{ name: 'Ann', email: 'ann@example.org' }, { name: null, email: 'bert@example.org' }])
    await fixture.whenStable()
    key('Backspace')
    await fixture.whenStable()
    expect(chips()).toEqual(['Ann'])
    el.querySelector<HTMLButtonElement>('button[aria-label="Remove ann@example.org"]')!.click()
    await fixture.whenStable()
    expect(chips()).toEqual([])
  })
})
