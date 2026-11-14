// Pure state handling: what a /queryChanges answer, a patch object and a
// keyword or mailbox change do to the data the client holds.

import type { Email, Id, Mailbox } from './types'

// applyQueryChanges applies a /queryChanges answer to the window of a
// query result the client holds (the first ids.length ids): removed first,
// then added in order (RFC 8620 5.6). An addition past the end of the
// window belongs to a page that is not loaded and is left out.
export function applyQueryChanges(ids: Id[], removed: Id[], added: { id: Id; index: number }[]): Id[] {
  const gone = new Set(removed)
  const out = ids.filter((id) => !gone.has(id))
  for (const a of [...added].sort((x, y) => x.index - y.index)) {
    if (a.index > out.length) continue
    const at = out.indexOf(a.id)
    if (at !== -1) out.splice(at, 1)
    out.splice(a.index, 0, a.id)
  }
  return out
}

export type EmailPatch = Record<string, true | null | Record<string, true>>

// applyPatch returns an email with a patch of Email/set applied
// (RFC 8620 5.3): "keywords/<k>" and "mailboxIds/<id>" set to true or
// null, or either map replaced as a whole.
export function applyPatch<T extends Pick<Email, 'keywords' | 'mailboxIds'>>(email: T, patch: EmailPatch): T {
  const out = { ...email, keywords: { ...email.keywords }, mailboxIds: { ...email.mailboxIds } }
  for (const [path, value] of Object.entries(patch)) {
    const slash = path.indexOf('/')
    const prop = slash === -1 ? path : path.slice(0, slash)
    if (prop !== 'keywords' && prop !== 'mailboxIds') throw new Error(`cannot patch ${path}`)
    if (slash === -1) {
      if (value === null || value === true) throw new Error(`${path} needs a map`)
      out[prop] = { ...value }
      continue
    }
    // A JSON pointer: "~1" is "/", "~0" is "~".
    const key = path.slice(slash + 1).replace(/~1/g, '/').replace(/~0/g, '~')
    if (value === true) out[prop][key] = true
    else if (value === null) delete out[prop][key]
    else throw new Error(`${path} must be true or null`)
  }
  return out
}

type Counted = Pick<Mailbox, 'totalEmails' | 'unreadEmails'>

// adjustCounts moves the email counters of the mailboxes from an email's
// old form to its new one. null is "does not exist": before for a new
// email, after for a destroyed one. Thread counters are not touched; the
// server's Mailbox/changes brings those.
export function adjustCounts(
  mailboxes: Record<Id, Counted>,
  before: Pick<Email, 'keywords' | 'mailboxIds'> | null,
  after: Pick<Email, 'keywords' | 'mailboxIds'> | null,
): void {
  const add = (e: Pick<Email, 'keywords' | 'mailboxIds'> | null, n: number) => {
    if (!e) return
    const unread = !e.keywords.$seen && !e.keywords.$draft
    for (const id of Object.keys(e.mailboxIds)) {
      const m = mailboxes[id]
      if (!m) continue
      m.totalEmails = Math.max(0, m.totalEmails + n)
      if (unread) m.unreadEmails = Math.max(0, m.unreadEmails + n)
    }
  }
  add(before, -1)
  add(after, 1)
}

// mailboxTree orders mailboxes for display: role mailboxes first in a fixed
// order, then by sortOrder and name; children under their parent.
const roleOrder = ['inbox', 'drafts', 'sent', 'archive', 'junk', 'trash']

export interface MailboxNode<M> {
  mailbox: M
  depth: number
  children: MailboxNode<M>[]
}

export function mailboxTree<M extends Pick<Mailbox, 'id' | 'parentId' | 'role' | 'sortOrder' | 'name'>>(list: M[]): MailboxNode<M>[] {
  const rank = (m: M) => {
    const i = m.role ? roleOrder.indexOf(m.role) : -1
    return i === -1 ? (m.role ? roleOrder.length : roleOrder.length + 1) : i
  }
  const sorted = [...list].sort(
    (a, b) => rank(a) - rank(b) || a.sortOrder - b.sortOrder || a.name.localeCompare(b.name, undefined, { sensitivity: 'base' }),
  )
  const ids = new Set(list.map((m) => m.id))
  const build = (parent: Id | null, depth: number): MailboxNode<M>[] =>
    sorted
      // A mailbox whose parent we do not know is shown at the top.
      .filter((m) => (parent === null ? m.parentId === null || !ids.has(m.parentId) : m.parentId === parent))
      .map((m) => ({ mailbox: m, depth, children: depth < 20 ? build(m.id, depth + 1) : [] }))
  return build(null, 0)
}

export function flattenTree<M>(nodes: MailboxNode<M>[]): MailboxNode<M>[] {
  return nodes.flatMap((n) => [n, ...flattenTree(n.children)])
}
