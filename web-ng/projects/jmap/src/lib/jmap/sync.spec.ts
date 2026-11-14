import { describe, expect, it } from 'vitest'
import { adjustCounts, applyPatch, applyQueryChanges, flattenTree, mailboxTree } from './sync'

describe('applyQueryChanges (RFC 8620 5.6)', () => {
  it('removes first, then adds at the given index in order', () => {
    expect(applyQueryChanges(['a', 'b', 'c'], ['b'], [])).toEqual(['a', 'c'])
    expect(applyQueryChanges(['a', 'b', 'c'], [], [{ id: 'n', index: 0 }])).toEqual(['n', 'a', 'b', 'c'])
    // An email that changed is reported as removed and added back.
    expect(applyQueryChanges(['a', 'b', 'c'], ['b'], [{ id: 'b', index: 1 }])).toEqual(['a', 'b', 'c'])
    // Two additions: indexes are those in the final list.
    expect(applyQueryChanges(['a', 'b'], [], [{ id: 'y', index: 2 }, { id: 'x', index: 0 }])).toEqual(['x', 'a', 'y', 'b'])
  })

  it('a thread whose newest email changed: the old row goes, the new one comes', () => {
    expect(applyQueryChanges(['e1', 'e5'], ['e1', 'e9'], [{ id: 'e9', index: 0 }])).toEqual(['e9', 'e5'])
  })

  it('leaves out additions past the loaded window, takes one at its end', () => {
    expect(applyQueryChanges(['a', 'b'], [], [{ id: 'z', index: 7 }])).toEqual(['a', 'b'])
    expect(applyQueryChanges(['a', 'b'], [], [{ id: 'z', index: 2 }])).toEqual(['a', 'b', 'z'])
  })

  it('does not duplicate an id the server adds without removing', () => {
    expect(applyQueryChanges(['a', 'b', 'c'], [], [{ id: 'c', index: 0 }])).toEqual(['c', 'a', 'b'])
  })
})

describe('applyPatch (RFC 8620 5.3)', () => {
  const email = { keywords: { $seen: true as const }, mailboxIds: { m1: true as const } }

  it('sets and removes single keys and leaves the original alone', () => {
    const out = applyPatch(email, { 'keywords/$flagged': true, 'keywords/$seen': null, 'mailboxIds/m2': true })
    expect(out).toEqual({ keywords: { $flagged: true }, mailboxIds: { m1: true, m2: true } })
    expect(email).toEqual({ keywords: { $seen: true }, mailboxIds: { m1: true } })
  })

  it('replaces a whole map', () => {
    expect(applyPatch(email, { mailboxIds: { m5: true } }).mailboxIds).toEqual({ m5: true })
  })

  it('unescapes JSON pointer tokens', () => {
    expect(applyPatch(email, { 'keywords/a~1b~0c': true }).keywords).toEqual({ $seen: true, 'a/b~c': true })
  })

  it('refuses other properties', () => {
    expect(() => applyPatch(email, { subject: { x: true } })).toThrow()
    expect(() => applyPatch(email, { mailboxIds: null })).toThrow()
  })
})

describe('adjustCounts', () => {
  const boxes = () => ({ m1: { totalEmails: 3, unreadEmails: 2 }, m2: { totalEmails: 0, unreadEmails: 0 } })
  const unread = { keywords: {}, mailboxIds: { m1: true as const } }

  it('marking read lowers unread only', () => {
    const b = boxes()
    adjustCounts(b, unread, applyPatch(unread, { 'keywords/$seen': true }))
    expect(b.m1).toEqual({ totalEmails: 3, unreadEmails: 1 })
  })

  it('a move carries total and unread along', () => {
    const b = boxes()
    adjustCounts(b, unread, applyPatch(unread, { 'mailboxIds/m1': null, 'mailboxIds/m2': true }))
    expect(b).toEqual({ m1: { totalEmails: 2, unreadEmails: 1 }, m2: { totalEmails: 1, unreadEmails: 1 } })
  })

  it('a label counts in both, a destroy in none; unknown mailboxes are skipped; never below zero', () => {
    const b = boxes()
    adjustCounts(b, unread, applyPatch(unread, { 'mailboxIds/m2': true, 'mailboxIds/m9': true }))
    expect(b.m2).toEqual({ totalEmails: 1, unreadEmails: 1 })
    adjustCounts(b, { keywords: {}, mailboxIds: { m2: true } }, null)
    adjustCounts(b, { keywords: {}, mailboxIds: { m2: true } }, null)
    expect(b.m2).toEqual({ totalEmails: 0, unreadEmails: 0 })
  })
})

describe('mailboxTree', () => {
  const m = (id: string, name: string, role: string | null = null, parentId: string | null = null, sortOrder = 0) => ({ id, name, role, parentId, sortOrder })

  it('puts roles first in a fixed order, then names; children under parents', () => {
    const tree = mailboxTree([
      m('a', 'Zebra'), m('t', 'Trash', 'trash'), m('b', 'apple'), m('i', 'Inbox', 'inbox'),
      m('c', 'Child', null, 'a'), m('g', 'Grandchild', null, 'c'), m('s', 'Sent', 'sent'), m('o', 'Lost', null, 'gone'),
    ])
    expect(flattenTree(tree).map((n) => [n.mailbox.name, n.depth])).toEqual([
      ['Inbox', 0], ['Sent', 0], ['Trash', 0], ['apple', 0], ['Lost', 0], ['Zebra', 0], ['Child', 1], ['Grandchild', 2],
    ])
  })

  it('sortOrder goes before the name', () => {
    expect(mailboxTree([m('a', 'A', null, null, 2), m('b', 'B', null, null, 1)]).map((n) => n.mailbox.id)).toEqual(['b', 'a'])
  })
})
