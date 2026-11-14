import type { EmailAddress } from '../jmap/types'

const time = new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit' })
const day = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'short' })
const dayYear = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'short', year: 'numeric' })
const full = new Intl.DateTimeFormat(undefined, { dateStyle: 'full', timeStyle: 'short' })

// shortDate is the date of a list row: the time today, the day this year,
// the day and year before that.
export function shortDate(iso: string, now = new Date()): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  if (d.toDateString() === now.toDateString()) return time.format(d)
  if (d.getFullYear() === now.getFullYear()) return day.format(d)
  return dayYear.format(d)
}

export function fullDate(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? '' : full.format(d)
}

// displayName is how a correspondent shows in a list: the name, or the
// address when there is none.
export function displayName(a: EmailAddress): string {
  return a.name?.trim() || a.email
}

export function addressList(list: EmailAddress[] | null | undefined): string {
  return (list ?? []).map((a) => (a.name?.trim() ? `${a.name.trim()} <${a.email}>` : a.email)).join(', ')
}

export function fileSize(bytes: number): string {
  if (bytes < 1000) return `${bytes} B`
  if (bytes < 1_000_000) return `${(bytes / 1000).toFixed(bytes < 10_000 ? 1 : 0)} kB`
  return `${(bytes / 1_000_000).toFixed(bytes < 10_000_000 ? 1 : 0)} MB`
}

const roleIcons: Record<string, string> = {
  inbox: 'i-lucide-inbox',
  drafts: 'i-lucide-file-pen',
  sent: 'i-lucide-send',
  archive: 'i-lucide-archive',
  junk: 'i-lucide-shield-alert',
  trash: 'i-lucide-trash-2',
}

export function mailboxIcon(role: string | null): string {
  return (role && roleIcons[role]) || 'i-lucide-folder'
}
