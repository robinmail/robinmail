// Addresses as a person types or pastes them: "Ann <ann@example.org>",
// "ann@example.org, bob@example.org", a column copied from a spreadsheet.

import type { EmailAddress } from '../jmap/types'

const atom = "[A-Za-z0-9!#$%&'*+/=?^_`{|}~\\u0080-\\uffff-]+"
const local = new RegExp(`^${atom}(\\.${atom})*$`)
const label = /^[A-Za-z0-9\u0080-\uffff]([A-Za-z0-9\u0080-\uffff-]{0,61}[A-Za-z0-9\u0080-\uffff])?$/

// isValidEmail accepts what a mail server takes as "local@domain": a
// dot-atom local part (RFC 5322 3.4.1, without quoted strings, which
// nobody types) and a domain of labels. A single label is allowed:
// "postmaster@localhost" is an address.
export function isValidEmail(s: string): boolean {
  const at = s.lastIndexOf('@')
  if (at < 1 || at === s.length - 1 || s.length > 254) return false
  const l = s.slice(0, at)
  const d = s.slice(at + 1)
  return l.length <= 64 && local.test(l) && d.split('.').every((x) => label.test(x))
}

// split cuts a list at commas, semicolons and line ends that are not
// inside a quoted string or angle brackets.
function split(input: string): string[] {
  const out: string[] = []
  let cur = ''
  let quoted = false
  let angle = false
  for (let i = 0; i < input.length; i++) {
    const ch = input[i]!
    if (quoted) {
      cur += ch
      if (ch === '\\' && i + 1 < input.length) cur += input[++i]
      else if (ch === '"') quoted = false
      continue
    }
    if (ch === '"') quoted = true
    else if (ch === '<') angle = true
    else if (ch === '>') angle = false
    if (!angle && (ch === ',' || ch === ';' || ch === '\n' || ch === '\r' || ch === '\t')) {
      out.push(cur)
      cur = ''
    } else cur += ch
  }
  out.push(cur)
  return out.map((s) => s.trim()).filter((s) => s !== '')
}

function unquote(name: string): string {
  const n = name.trim()
  if (n.length >= 2 && n.startsWith('"') && n.endsWith('"')) return n.slice(1, -1).replace(/\\(.)/g, '$1').trim()
  return n
}

export interface Parsed {
  addresses: EmailAddress[]
  // The pieces that are not an address, as typed.
  invalid: string[]
}

// parseAddresses reads a typed or pasted list. A piece is "Name <address>",
// "<address>", "address", or several bare addresses separated by spaces;
// "mailto:" in front of an address is dropped.
export function parseAddresses(input: string): Parsed {
  const addresses: EmailAddress[] = []
  const invalid: string[] = []
  const add = (name: string | null, email: string, raw: string) => {
    const e = email.trim().replace(/^mailto:/i, '')
    if (isValidEmail(e)) addresses.push({ name: name || null, email: e })
    else invalid.push(raw)
  }
  for (const piece of split(input)) {
    const m = /^(.*)<([^<>]*)>$/.exec(piece)
    if (m) {
      add(unquote(m[1]!), m[2]!, piece)
      continue
    }
    const words = piece.split(/\s+/)
    if (words.length > 1 && words.every((w) => w.includes('@'))) {
      for (const w of words) add(null, w, w)
      continue
    }
    add(null, piece, piece)
  }
  return { addresses: dedupe(addresses), invalid }
}

// sameAddress compares two addresses the way mail servers treat them in
// practice: without regard to case.
export function sameAddress(a: string, b: string): boolean {
  return a.toLowerCase() === b.toLowerCase()
}

// dedupe keeps the first of every address.
export function dedupe(list: EmailAddress[]): EmailAddress[] {
  const seen = new Set<string>()
  return list.filter((a) => {
    const k = a.email.toLowerCase()
    if (seen.has(k)) return false
    seen.add(k)
    return true
  })
}

// formatAddress writes an address for a text field or a quoted header.
export function formatAddress(a: EmailAddress): string {
  const name = a.name?.trim()
  if (!name) return a.email
  return /^[A-Za-z0-9 \u0080-\uffff'._-]+$/.test(name) ? `${name} <${a.email}>` : `"${name.replace(/(["\\])/g, '\\$1')}" <${a.email}>`
}

export interface Mailto {
  to: EmailAddress[]
  cc: EmailAddress[]
  bcc: EmailAddress[]
  subject: string
  body: string
}

// parseMailto reads a "mailto:" URL (RFC 6068): addresses in the path and
// in "to", and the fields subject, body, cc and bcc. Other fields are
// ignored, as the RFC asks for anything that could be unsafe.
export function parseMailto(url: string): Mailto | null {
  if (!/^mailto:/i.test(url)) return null
  const rest = url.slice(7)
  const q = rest.indexOf('?')
  const dec = (s: string) => {
    try {
      return decodeURIComponent(s)
    } catch {
      return s
    }
  }
  const out = { to: [dec(q === -1 ? rest : rest.slice(0, q))], cc: [] as string[], bcc: [] as string[], subject: '', body: '' }
  if (q !== -1) {
    for (const pair of rest.slice(q + 1).split('&')) {
      const eq = pair.indexOf('=')
      const key = dec(eq === -1 ? pair : pair.slice(0, eq)).toLowerCase()
      const value = dec(eq === -1 ? '' : pair.slice(eq + 1))
      if (key === 'to' || key === 'cc' || key === 'bcc') out[key].push(value)
      else if (key === 'subject') out.subject = value
      else if (key === 'body') out.body = value.replace(/\r\n/g, '\n')
    }
  }
  return {
    to: parseAddresses(out.to.join(',')).addresses,
    cc: parseAddresses(out.cc.join(',')).addresses,
    bcc: parseAddresses(out.bcc.join(',')).addresses,
    subject: out.subject,
    body: out.body,
  }
}
