import type { EmailAddress } from '@robinmail/jmap'

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
  inbox: 'inbox',
  drafts: 'file-pen',
  sent: 'send',
  archive: 'archive',
  junk: 'shield-alert',
  trash: 'trash-2',
}

export function mailboxIcon(role: string | null): string {
  return (role && roleIcons[role]) || 'folder'
}

// octets writes a size in the unit a person reads it in, with binary
// prefixes as the server counts: 1536 is "1.5 KB".
export function octets(n: number): string {
  if (n < 1024) return `${n} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let v = n / 1024
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v >= 100 || Number.isInteger(v) ? Math.round(v) : v.toFixed(1)} ${units[i]}`
}
