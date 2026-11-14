// The rules of the Filters page, and the Sieve script (RFC 5228) they are
// kept as. The page has no storage of its own: what it shows is read out
// of the account's active script, and what it saves is a script.
//
// A script the page wrote has a fixed shape, which parse reads back. Any
// other script is somebody's own: parse answers null and the page shows
// it as text only. The test of "is it ours" is not a pattern but a round
// trip: a script is rules exactly when writing those rules gives the
// script again.

export type Field = 'from' | 'to' | 'subject'

export interface Condition {
  field: Field
  text: string
}

export type Action =
  | { kind: 'move'; mailbox: string }
  | { kind: 'label'; mailbox: string }
  | { kind: 'read' }
  | { kind: 'forward'; address: string }
  | { kind: 'delete' }

export interface Rule {
  name: string
  // all: every condition must hold; any: one is enough.
  match: 'all' | 'any'
  conditions: Condition[]
  actions: Action[]
}

export const MARK = '# robinmail rules 1'
const NOTE = [
  '# Written by the Filters page of the webmail. The page reads it back as',
  '# rules; a script changed by hand is shown there as a script only.',
]

// The order actions are written in, whatever order they were chosen in:
// flag first, so that every copy carries it; what ends the rule last.
const ORDER: Action['kind'][] = ['read', 'label', 'forward', 'move', 'delete']

// quote writes a Sieve quoted string (RFC 5228 2.4.2).
export function quote(s: string): string {
  return `"${s.replace(/\\/g, '\\\\').replace(/"/g, '\\"')}"`
}

function unquote(s: string): string {
  return s.slice(1, -1).replace(/\\(.)/g, '$1')
}

function condition(c: Condition): string {
  switch (c.field) {
    case 'from':
      return `address :contains "from" ${quote(c.text)}`
    case 'to':
      return `address :contains ["to", "cc"] ${quote(c.text)}`
    case 'subject':
      return `header :contains "subject" ${quote(c.text)}`
  }
}

function action(a: Action): string {
  switch (a.kind) {
    case 'read':
      return 'addflag "\\\\Seen";'
    case 'label':
      return `fileinto :copy :create ${quote(a.mailbox)};`
    case 'forward':
      return `redirect :copy ${quote(a.address)};`
    case 'move':
      return `fileinto :create ${quote(a.mailbox)};`
    case 'delete':
      return 'discard;'
  }
}

function sorted(actions: Action[]): Action[] {
  return [...actions].sort((a, b) => ORDER.indexOf(a.kind) - ORDER.indexOf(b.kind))
}

// A rule that moves or deletes a message is the last one that message
// sees; after any other rule the next ones still run.
function ends(r: Rule): boolean {
  return r.actions.some((a) => a.kind === 'move' || a.kind === 'delete')
}

// problem says what is wrong with a rule, or '' when it can be saved.
export function problem(r: Rule): string {
  const plain = (s: string) => !/[\r\n\0]/.test(s)
  if (!r.name.trim()) return 'Give the rule a name.'
  if (!plain(r.name)) return 'The name must be one line.'
  if (r.conditions.length === 0) return 'A rule needs something to look for.'
  for (const c of r.conditions) {
    if (!c.text.trim()) return 'Say what to look for in every condition.'
    if (!plain(c.text)) return 'What to look for must be one line.'
  }
  if (r.actions.length === 0) return 'Choose what to do with the message.'
  const kinds = new Set(r.actions.map((a) => a.kind))
  if (kinds.size !== r.actions.length) return 'Each thing to do can be chosen once.'
  for (const a of r.actions) {
    if ((a.kind === 'move' || a.kind === 'label') && (!a.mailbox.trim() || !plain(a.mailbox))) return 'Choose a mailbox.'
    if (a.kind === 'forward' && !/^[^\s@<>"]+@[^\s@<>"]+\.[^\s@<>"]+$/.test(a.address)) return 'Forward to one email address.'
  }
  if (kinds.has('move') && kinds.has('delete')) return 'A message cannot be moved and deleted.'
  return ''
}

// generate writes the script for a list of rules, with LF line ends.
export function generate(rules: Rule[]): string {
  const need = new Set<string>()
  for (const r of rules) {
    for (const a of r.actions) {
      if (a.kind === 'move' || a.kind === 'label') need.add('fileinto').add('mailbox')
      if (a.kind === 'label' || a.kind === 'forward') need.add('copy')
      if (a.kind === 'read') need.add('imap4flags')
    }
  }
  const lines = [MARK, ...NOTE]
  const require = ['fileinto', 'mailbox', 'copy', 'imap4flags'].filter((x) => need.has(x))
  if (require.length) lines.push(`require [${require.map(quote).join(', ')}];`)
  for (const r of rules) {
    lines.push('', `# rule: ${r.name.trim()}`)
    const tests = r.conditions.map(condition)
    const test = tests.length === 1 ? tests[0]! : `${r.match === 'any' ? 'anyof' : 'allof'} (${tests.join(', ')})`
    lines.push(`if ${test} {`)
    for (const a of sorted(r.actions)) lines.push(`    ${action(a)}`)
    if (ends(r)) lines.push('    stop;')
    lines.push('}')
  }
  return lines.join('\n') + '\n'
}

const STRING = '"(?:[^"\\\\]|\\\\.)*"'
const CONDITION = new RegExp(`^(?:address :contains "from" (${STRING})|address :contains \\["to", "cc"\\] (${STRING})|header :contains "subject" (${STRING}))`)

function parseConditions(test: string): { match: 'all' | 'any'; conditions: Condition[] } | null {
  let match: 'all' | 'any' = 'all'
  let rest = test
  const list = /^(allof|anyof) \((.*)\)$/.exec(test)
  if (list) {
    match = list[1] === 'anyof' ? 'any' : 'all'
    rest = list[2]!
  }
  const conditions: Condition[] = []
  for (;;) {
    const m = CONDITION.exec(rest)
    if (!m) return null
    if (m[1] !== undefined) conditions.push({ field: 'from', text: unquote(m[1]) })
    else if (m[2] !== undefined) conditions.push({ field: 'to', text: unquote(m[2]) })
    else conditions.push({ field: 'subject', text: unquote(m[3]!) })
    rest = rest.slice(m[0].length)
    if (rest === '') break
    if (!list || !rest.startsWith(', ')) return null
    rest = rest.slice(2)
  }
  return { match, conditions }
}

function parseAction(line: string): Action | null {
  let m: RegExpExecArray | null
  if (line === 'addflag "\\\\Seen";') return { kind: 'read' }
  if (line === 'discard;') return { kind: 'delete' }
  if ((m = new RegExp(`^fileinto :copy :create (${STRING});$`).exec(line))) return { kind: 'label', mailbox: unquote(m[1]!) }
  if ((m = new RegExp(`^fileinto :create (${STRING});$`).exec(line))) return { kind: 'move', mailbox: unquote(m[1]!) }
  if ((m = new RegExp(`^redirect :copy (${STRING});$`).exec(line))) return { kind: 'forward', address: unquote(m[1]!) }
  return null
}

// parse reads a script the page wrote back into rules. It answers null
// for every other script, also one that only differs in a space.
export function parse(script: string): Rule[] | null {
  const text = script.replace(/\r\n/g, '\n')
  if (!text.startsWith(MARK + '\n')) return null
  const rules: Rule[] = []
  let rule: Rule | null = null
  let open = false
  for (const line of text.split('\n')) {
    const name = /^# rule: (.*)$/.exec(line)
    if (name && !open) {
      rule = { name: name[1]!, match: 'all', conditions: [], actions: [] }
      continue
    }
    const head = /^if (.*) \{$/.exec(line)
    if (head && rule && !open) {
      const c = parseConditions(head[1]!)
      if (!c) return null
      rule.match = c.match
      rule.conditions = c.conditions
      open = true
      continue
    }
    if (line === '}' && rule && open) {
      rules.push(rule)
      rule = null
      open = false
      continue
    }
    if (open && rule && line.startsWith('    ') && line !== '    stop;') {
      const a = parseAction(line.slice(4))
      if (!a) return null
      rule.actions.push(a)
    }
  }
  // The proof: these rules, written out, are this script.
  return generate(rules) === text ? rules : null
}

// describe says in words what a rule does, for the list.
export function describe(r: Rule): { when: string; then: string } {
  const field = { from: 'From', to: 'To or Cc', subject: 'Subject' }
  const when = r.conditions.map((c) => `${field[c.field]} contains “${c.text}”`).join(r.match === 'any' ? ' or ' : ' and ')
  const then = sorted(r.actions).map((a) => {
    switch (a.kind) {
      case 'read':
        return 'mark as read'
      case 'label':
        return `label ${a.mailbox}`
      case 'forward':
        return `forward to ${a.address}`
      case 'move':
        return `move to ${a.mailbox}`
      case 'delete':
        return 'delete'
    }
  }).join(', ')
  return { when, then }
}
