// What the three panes share: the route read as a list spec, navigation,
// the rows of the list, the selection, and the actions on rows.

import { computed, ref } from 'vue'
import { useRoute, useRouter, type LocationQueryRaw } from 'vue-router'
import { store } from '../app'
import { flattenTree, mailboxTree } from '../jmap/sync'
import type { Email, Id, Mailbox } from '../jmap/types'
import type { ListSpec } from '../store/mail'
import { displayName } from './format'

// A Row is one thread in the list.
export interface Row {
  // The email the query returned for the thread.
  id: Id
  email: Email
  threadId: Id
  // Every email of the thread that is loaded, oldest first.
  emails: Email[]
  // Those in the mailbox of the list: what move and delete act on.
  scope: Email[]
  unread: boolean
  flagged: boolean
  attachment: boolean
  names: string
}

export function buildRow(id: Id, mailboxId: Id, showRecipients: boolean): Row | null {
  const email = store.state.emails[id]
  if (!email) return null
  const thread = store.threadEmails(email.threadId)
  const emails = thread.some((e) => e.id === id) ? thread : [email]
  const scope = emails.filter((e) => e.mailboxIds[mailboxId])
  const names = new Set<string>()
  for (const e of emails) for (const a of (showRecipients ? e.to : e.from) ?? []) names.add(displayName(a))
  return {
    id,
    email,
    threadId: email.threadId,
    emails,
    scope: scope.length ? scope : [email],
    unread: emails.some((e) => !e.keywords.$seen),
    flagged: emails.some((e) => e.keywords.$flagged),
    attachment: emails.some((e) => e.hasAttachment),
    names: (showRecipients ? 'To: ' : '') + ([...names].join(', ') || '(nobody)'),
  }
}

const ids = (list: Email[]) => list.map((e) => e.id)

// The selected rows of the list, by row id. Empty again in another list.
export const selection = ref(new Set<Id>())

export const actions = {
  // Read: every email of the threads. Unread: the email of the row.
  setRead(rows: Row[], read: boolean) {
    return store.setKeyword(read ? rows.flatMap((r) => ids(r.emails)) : rows.map((r) => r.id), '$seen', read)
  },
  toggleRead(rows: Row[]) {
    return this.setRead(rows, rows.some((r) => r.unread))
  },
  // Flag: the email of the row. Unflag: every email of the threads.
  toggleFlag(rows: Row[]) {
    const flag = rows.some((r) => !r.flagged)
    return store.setKeyword(flag ? rows.filter((r) => !r.flagged).map((r) => r.id) : rows.flatMap((r) => ids(r.emails)), '$flagged', flag)
  },
  move(rows: Row[], from: Id, to: Id) {
    return store.move(rows.flatMap((r) => ids(r.scope)), from, to)
  },
  remove(rows: Row[]) {
    return store.remove(rows.flatMap((r) => ids(r.scope)))
  },
}

export interface MailboxOption {
  mailbox: Mailbox
  depth: number
}

export function useMailView() {
  const route = useRoute()
  const router = useRouter()

  const param = (name: string) => {
    const v = route.params[name]
    return typeof v === 'string' ? v : ''
  }
  const query = (name: string) => {
    const v = route.query[name]
    return typeof v === 'string' ? v : ''
  }

  const mailboxId = computed(() => param('mailbox'))
  const threadId = computed(() => param('thread'))
  const mailbox = computed<Mailbox | undefined>(() => store.state.mailboxes[mailboxId.value])
  const spec = computed<ListSpec>(() => ({
    mailboxId: mailboxId.value,
    text: query('q'),
    from: query('from'),
    subject: query('subject'),
    unread: query('unread') === '1',
    flagged: query('flagged') === '1',
    attachment: query('attachment') === '1',
  }))
  const filterCount = computed(() => {
    const s = spec.value
    return [s.text, s.from, s.subject, s.unread, s.flagged, s.attachment].filter(Boolean).length
  })

  // All mailboxes in display order, with their depth.
  const mailboxes = computed<MailboxOption[]>(() =>
    flattenTree(mailboxTree(Object.values(store.state.mailboxes))).map((n) => ({ mailbox: n.mailbox, depth: n.depth })),
  )

  const rows = computed<Row[]>(() => {
    const showRecipients = mailbox.value?.role === 'sent' || mailbox.value?.role === 'drafts'
    return store.state.list.ids.map((id) => buildRow(id, mailboxId.value, showRecipients)).filter((r): r is Row => !!r)
  })
  const currentRow = computed(() => rows.value.find((r) => r.threadId === threadId.value))
  const selectedRows = computed(() => rows.value.filter((r) => selection.value.has(r.id)))

  function specQuery(s: ListSpec): LocationQueryRaw {
    return {
      ...(s.text ? { q: s.text } : {}),
      ...(s.from ? { from: s.from } : {}),
      ...(s.subject ? { subject: s.subject } : {}),
      ...(s.unread ? { unread: '1' } : {}),
      ...(s.flagged ? { flagged: '1' } : {}),
      ...(s.attachment ? { attachment: '1' } : {}),
    }
  }

  function threadLocation(thread: Id) {
    // On the way to another page the route has no mailbox for a moment.
    return { name: 'mail', params: { mailbox: mailboxId.value || null, thread }, query: route.query }
  }
  // Opening the first thread adds a history entry, so "back" returns to
  // the list; going from thread to thread replaces it.
  function openThread(thread: Id) {
    return threadId.value ? router.replace(threadLocation(thread)) : router.push(threadLocation(thread))
  }
  function closeThread() {
    return router.replace({ name: 'mail', params: { mailbox: mailboxId.value }, query: route.query })
  }
  function setFilter(change: Partial<ListSpec>) {
    return router.replace({
      name: 'mail',
      params: { mailbox: mailboxId.value, ...(threadId.value ? { thread: threadId.value } : {}) },
      query: specQuery({ ...spec.value, ...change }),
    })
  }

  // step opens the thread after (1) or before (-1) the open one.
  function step(by: 1 | -1) {
    const list = rows.value
    if (list.length === 0) return
    const at = list.findIndex((r) => r.threadId === threadId.value)
    const next = at === -1 ? (by === 1 ? 0 : list.length - 1) : at + by
    const row = list[next]
    if (row) void openThread(row.threadId)
  }

  // leave is called when the open thread is moved or deleted: back to the
  // list. It does not open the next thread: that would mark it read
  // without the user having asked for it.
  function leave() {
    return closeThread()
  }

  return { mailboxId, threadId, mailbox, spec, filterCount, mailboxes, rows, currentRow, selectedRows, openThread, closeThread, setFilter, step, leave, threadLocation }
}
