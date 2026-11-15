import { ChangeDetectionStrategy, Component, provideZonelessChangeDetection } from '@angular/core'
import { TestBed } from '@angular/core/testing'
import { reactive } from '@robinmail/jmap'
import { describe, expect, it } from 'vitest'
import { text } from './testing/helpers'

const state = reactive({ list: { ids: ['a'] as string[] }, names: {} as Record<string, string> })

@Component({
  selector: 'rm-probe',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `@for (id of state.list.ids; track id) {<i>{{ state.names[id] ?? id }}</i>}`,
})
class Probe {
  protected readonly state = state
}

describe('a template over reactive state', () => {
  it('is drawn again when the state is written to, without anyone telling the component', async () => {
    TestBed.configureTestingModule({ providers: [provideZonelessChangeDetection()] })
    const fixture = TestBed.createComponent(Probe)
    await fixture.whenStable()
    expect(text(fixture.nativeElement)).toBe('a')

    state.list.ids.push('b')
    state.names['a'] = 'Ann'
    await fixture.whenStable()
    expect(text(fixture.nativeElement)).toBe('Annb')

    delete state.names['a']
    state.list.ids = ['b']
    await fixture.whenStable()
    expect(text(fixture.nativeElement)).toBe('b')
  })
})
