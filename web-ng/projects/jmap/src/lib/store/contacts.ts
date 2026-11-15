// The contacts store: the address books of every account the user sees,
// their cards, and the actions on them. Cards are few enough to load
// whole: a search is done here, as the user types.

import { reactive } from '../reactive'
import type { JmapClient } from '../jmap/client'
import type { AddressBook, ContactCard, Id } from '../jmap/types'

export const CONTACTS = 'urn:ietf:params:jmap:contacts'

export interface BookView {
  accountId: Id
  owner: string
  own: boolean
  book: AddressBook
}

export interface CardView {
  accountId: Id
  bookId: Id
  card: ContactCard
  name: string
  sortKey: string
}

// ContactDraft is what the editor works on.
export interface ContactDraft {
  accountId: Id
  bookId: Id
  given: string
  surname: string
  organization: string
  title: string
  emails: { address: string; context: '' | 'work' | 'private' }[]
  phones: { number: string; kind: '' | 'mobile' | 'work' | 'home' }[]
  address: string
  birthday: string // yyyy-mm-dd, or --mm-dd
  note: string
}

export class ContactsError extends Error {}

// displayName is what a card is shown as.
export function displayName(c: ContactCard): string {
  const n = c.name
  if (n?.full) return n.full
  const parts = (n?.components ?? []).filter((x) => x.kind === 'given' || x.kind === 'given2' || x.kind === 'surname').map((x) => x.value)
  if (parts.length) return parts.join(' ')
  const org = Object.values(c.organizations ?? {})[0]?.name
  if (org) return org
  return Object.values(c.emails ?? {})[0]?.address ?? '(no name)'
}

function component(c: ContactCard, kind: string): string {
  return (c.name?.components ?? []).filter((x) => x.kind === kind).map((x) => x.value).join(' ')
}

export function draftOf(v: CardView): ContactDraft {
  const c = v.card
  const bday = Object.values(c.anniversaries ?? {}).find((a) => a.kind === 'birth')?.date
  const pad = (n: number) => String(n).padStart(2, '0')
  return {
    accountId: v.accountId,
    bookId: v.bookId,
    given: component(c, 'given'),
    surname: component(c, 'surname'),
    organization: Object.values(c.organizations ?? {})[0]?.name ?? '',
    title: Object.values(c.titles ?? {})[0]?.name ?? '',
    emails: Object.values(c.emails ?? {}).map((e) => ({ address: e.address, context: e.contexts?.['work'] ? 'work' : e.contexts?.['private'] ? 'private' : '' })),
    phones: Object.values(c.phones ?? {}).map((p) => ({
      number: p.number,
      kind: p.features?.['mobile'] ? 'mobile' : p.contexts?.['work'] ? 'work' : p.contexts?.['private'] ? 'home' : '',
    })),
    address: Object.values(c.addresses ?? {})[0]?.full ?? (Object.values(c.addresses ?? {})[0]?.components ?? []).map((x) => x.value).join(', '),
    birthday: bday ? (bday.year ? `${bday.year}-${pad(bday.month ?? 1)}-${pad(bday.day ?? 1)}` : `--${pad(bday.month ?? 1)}-${pad(bday.day ?? 1)}`) : '',
    note: Object.values(c.notes ?? {}).map((n) => n.note).join('\n\n'),
  }
}

// cardOf makes the JSContact properties of a draft.
export function cardOf(d: ContactDraft): Partial<ContactCard> {
  const card: Partial<ContactCard> = { '@type': 'Card', addressBookIds: { [d.bookId]: true } }
  const comps = []
  if (d.given.trim()) comps.push({ kind: 'given', value: d.given.trim() })
  if (d.surname.trim()) comps.push({ kind: 'surname', value: d.surname.trim() })
  card.name = comps.length ? { components: comps, full: comps.map((c) => c.value).join(' ') } : undefined
  card.organizations = d.organization.trim() ? { o1: { name: d.organization.trim() } } : undefined
  card.titles = d.title.trim() ? { t1: { name: d.title.trim() } } : undefined
  const emails: ContactCard['emails'] = {}
  d.emails.filter((e) => e.address.trim()).forEach((e, i) => {
    emails![`e${i + 1}`] = { address: e.address.trim(), ...(e.context ? { contexts: { [e.context]: true } } : {}) }
  })
  card.emails = Object.keys(emails).length ? emails : undefined
  const phones: ContactCard['phones'] = {}
  d.phones.filter((p) => p.number.trim()).forEach((p, i) => {
    const x: { number: string; features?: Record<string, boolean>; contexts?: Record<string, boolean> } = { number: p.number.trim() }
    if (p.kind === 'mobile') x.features = { mobile: true }
    if (p.kind === 'work') x.contexts = { work: true }
    if (p.kind === 'home') x.contexts = { private: true }
    phones![`p${i + 1}`] = x
  })
  card.phones = Object.keys(phones).length ? phones : undefined
  card.addresses = d.address.trim() ? { a1: { full: d.address.trim(), components: [{ kind: 'name', value: d.address.trim() }] } } : undefined
  const m = /^(?:(\d{4})|-)-(\d{2})-(\d{2})$/.exec(d.birthday.trim())
  card.anniversaries = m
    ? { birth: { kind: 'birth', date: { ...(m[1] ? { year: +m[1] } : {}), month: +m[2]!, day: +m[3]! } } }
    : undefined
  card.notes = d.note.trim() ? { n1: { note: d.note.trim() } } : undefined
  return card
}

export class ContactsStore {
  readonly state = reactive({
    ready: false,
    books: [] as BookView[],
    cards: [] as CardView[],
    error: '',
  })

  constructor(private client: JmapClient) {}

  private accounts(): { id: Id; name: string; own: boolean }[] {
    const s = this.client.session
    if (!s) return []
    const primary = s.primaryAccounts[CONTACTS] ?? this.client.accountId
    return Object.entries(s.accounts)
      .filter(([, a]) => CONTACTS in a.accountCapabilities)
      .map(([id, a]) => ({ id, name: a.name, own: id === primary }))
      .sort((a, b) => Number(b.own) - Number(a.own) || a.name.localeCompare(b.name))
  }

  async load(): Promise<void> {
    if (!this.client.session) await this.client.loadSession()
    const accounts = this.accounts()
    const b = this.client.batch()
    const calls = accounts.map((a) => ({
      books: b.call('AddressBook/get', { accountId: a.id }),
      cards: b.call('ContactCard/get', { accountId: a.id }),
    }))
    const res = await b.send()
    const books: BookView[] = []
    const cards: CardView[] = []
    accounts.forEach((a, i) => {
      for (const book of res.get(calls[i]!.books).list) books.push({ accountId: a.id, owner: a.name, own: a.own, book })
      for (const card of res.get(calls[i]!.cards).list) {
        const name = displayName(card)
        cards.push({ accountId: a.id, bookId: Object.keys(card.addressBookIds ?? {})[0] ?? '', card, name, sortKey: name.toLocaleLowerCase() })
      }
    })
    this.state.books = books
    this.state.cards = cards.sort((x, y) => x.sortKey.localeCompare(y.sortKey))
    this.state.ready = true
  }

  // search keeps the cards whose name, organization, addresses or numbers
  // hold the text.
  search(text: string): CardView[] {
    const t = text.trim().toLocaleLowerCase()
    if (!t) return this.state.cards
    return this.state.cards.filter((v) => {
      const c = v.card
      const hay = [v.name, ...Object.values(c.emails ?? {}).map((e) => e.address), ...Object.values(c.phones ?? {}).map((p) => p.number),
        ...Object.values(c.organizations ?? {}).map((o) => o.name ?? '')].join(' ').toLocaleLowerCase()
      return hay.includes(t)
    })
  }

  book(accountId: Id, bookId: Id): BookView | undefined {
    return this.state.books.find((b) => b.accountId === accountId && b.book.id === bookId)
  }

  async save(d: ContactDraft, existing?: CardView): Promise<Id> {
    const card = cardOf(d)
    const b = this.client.batch()
    if (existing) {
      if (existing.accountId !== d.accountId) throw new ContactsError('A contact cannot move to an address book of another account.')
      // Every property the editor knows is set or cleared.
      const patch: Record<string, unknown> = {}
      for (const k of ['name', 'organizations', 'titles', 'emails', 'phones', 'addresses', 'anniversaries', 'notes', 'addressBookIds'] as const) {
        patch[k] = (card as Record<string, unknown>)[k] ?? null
      }
      const s = b.call('ContactCard/set', { accountId: d.accountId, update: { [existing.card.id]: patch } })
      const res = (await b.send()).get(s)
      if (res.notUpdated?.[existing.card.id]) throw new ContactsError(res.notUpdated[existing.card.id]!.description ?? 'The contact was not saved')
      await this.load()
      return existing.card.id
    }
    const s = b.call('ContactCard/set', { accountId: d.accountId, create: { c: card } })
    const res = (await b.send()).get(s)
    if (!res.created?.['c']) throw new ContactsError(res.notCreated?.['c']?.description ?? 'The contact was not saved')
    await this.load()
    return res.created['c'].id
  }

  async remove(v: CardView): Promise<void> {
    const b = this.client.batch()
    const s = b.call('ContactCard/set', { accountId: v.accountId, destroy: [v.card.id] })
    const res = (await b.send()).get(s)
    if (res.notDestroyed?.[v.card.id]) throw new ContactsError(res.notDestroyed[v.card.id]!.description ?? 'The contact was not deleted')
    await this.load()
  }

  private davBase(v: BookView): string {
    const me = this.accounts().find((a) => a.own)?.id ?? ''
    const seg = v.own ? v.book.id : `${v.accountId}-${v.book.id}`
    return `/dav/addressbooks/${me}/${seg}/`
  }

  // exportURL is where an address book can be downloaded as one .vcf.
  exportURL(v: BookView): string {
    return this.davBase(v)
  }

  // importVCF stores every card of a .vcf file in an address book, through
  // CardDAV, which takes vCard as it is (RFC 6352 5.1).
  async importVCF(text: string, v: BookView, fetchFn: typeof fetch = fetch): Promise<number> {
    const cards = text.replace(/\r\n/g, '\n').split(/(?=^BEGIN:VCARD$)/im).map((s) => s.trim()).filter((s) => /^BEGIN:VCARD/i.test(s))
    if (!cards.length) throw new ContactsError('The file holds no contacts.')
    let n = 0
    for (const raw of cards) {
      let card = raw.replace(/\n/g, '\r\n') + '\r\n'
      if (!/^UID[;:]/im.test(card)) card = card.replace(/END:VCARD\r\n$/i, `UID:${crypto.randomUUID()}\r\nEND:VCARD\r\n`)
      const res = await fetchFn(this.davBase(v) + crypto.randomUUID() + '.vcf', {
        method: 'PUT', credentials: 'same-origin', headers: { 'Content-Type': 'text/vcard; charset=utf-8' }, body: card,
      })
      if (res.ok) n++
    }
    await this.load()
    return n
  }
}
