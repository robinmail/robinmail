import { describe, expect, it } from 'vitest'
import { cardOf, displayName, draftOf } from './contacts'
import type { ContactCard } from '../jmap/types'

describe('contacts', () => {
  it('turns a draft into JSContact (RFC 9553) and back', () => {
    const card = cardOf({
      accountId: 'a', bookId: 'b1', given: 'Jane', surname: 'Doe', organization: 'Acme', title: 'CTO',
      emails: [{ address: 'jane@example.com', context: 'work' }, { address: '', context: '' }],
      phones: [{ number: '+31 6 1234', kind: 'mobile' }], address: 'Main Street 1, Amsterdam', birthday: '--04-15', note: 'Met at a fair',
    })
    expect(card.name).toEqual({ components: [{ kind: 'given', value: 'Jane' }, { kind: 'surname', value: 'Doe' }], full: 'Jane Doe' })
    expect(card.emails).toEqual({ e1: { address: 'jane@example.com', contexts: { work: true } } })
    expect(card.phones).toEqual({ p1: { number: '+31 6 1234', features: { mobile: true } } })
    expect(card.anniversaries).toEqual({ birth: { kind: 'birth', date: { month: 4, day: 15 } } })
    const back = draftOf({ accountId: 'a', bookId: 'b1', card: { id: 'p1', ...card } as ContactCard, name: 'Jane Doe', sortKey: '' })
    expect(back.given).toBe('Jane')
    expect(back.birthday).toBe('--04-15')
    expect(back.phones).toEqual([{ number: '+31 6 1234', kind: 'mobile' }])
  })

  it('shows a card by its name, else its organization, else its address', () => {
    expect(displayName({ id: 'x', addressBookIds: {}, name: { full: 'Full Name' } })).toBe('Full Name')
    expect(displayName({ id: 'x', addressBookIds: {}, organizations: { o: { name: 'Acme' } } })).toBe('Acme')
    expect(displayName({ id: 'x', addressBookIds: {}, emails: { e: { address: 'a@b' } } })).toBe('a@b')
  })
})
