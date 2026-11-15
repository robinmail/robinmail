import { Component, provideZonelessChangeDetection, signal } from '@angular/core'
import { TestBed } from '@angular/core/testing'
import { describe, expect, it } from 'vitest'
import { text, until } from '../testing/helpers'
import { Select } from './select'

@Component({
  imports: [Select],
  template: `<label for="from">From</label><rm-select buttonId="from" [value]="value()" [options]="options" placeholder="Choose" (valueChange)="value.set($event)" />`,
})
class Host {
  readonly value = signal('')
  readonly options = [{ label: 'Inbox', value: 'INBOX' }, { label: 'Work/Invoices', value: 'Work/Invoices' }]
}

describe('the select', () => {
  it('shows the choice, opens a listbox of options and takes the one that is clicked', async () => {
    TestBed.configureTestingModule({ providers: [provideZonelessChangeDetection()] })
    const fixture = TestBed.createComponent(Host)
    await fixture.whenStable()
    const button = (fixture.nativeElement as HTMLElement).querySelector<HTMLButtonElement>('button#from')!
    expect(text(button)).toBe('Choose')
    expect(button.getAttribute('aria-expanded')).toBe('false')
    expect(document.querySelector('[role="listbox"]')).toBeNull()

    button.click()
    await until(() => !!document.querySelector('[role="listbox"]'), fixture)
    expect(button.getAttribute('aria-expanded')).toBe('true')
    const options = [...document.querySelectorAll<HTMLElement>('[role="option"]')]
    expect(options.map(text)).toEqual(['Inbox', 'Work/Invoices'])

    options[1]!.click()
    await until(() => !document.querySelector('[role="listbox"]'), fixture)
    expect(fixture.componentInstance.value()).toBe('Work/Invoices')
    expect(text(button)).toBe('Work/Invoices')

    // The chosen one is marked, and choosing it again changes nothing.
    button.click()
    await until(() => !!document.querySelector('[role="listbox"]'), fixture)
    const again = [...document.querySelectorAll<HTMLElement>('[role="option"]')]
    expect(again.map((o) => o.getAttribute('aria-selected'))).toEqual(['false', 'true'])
    again[1]!.click()
    await until(() => !document.querySelector('[role="listbox"]'), fixture)
    expect(fixture.componentInstance.value()).toBe('Work/Invoices')
  })
})
