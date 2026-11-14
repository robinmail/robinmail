import { readFileSync } from 'node:fs'
import { describe as suite, expect, it } from 'vitest'
import { MARK, describe, generate, parse, problem, quote, type Rule } from './rules'

const invoices: Rule = {
  name: 'Invoices',
  match: 'all',
  conditions: [{ field: 'from', text: 'billing@' }, { field: 'subject', text: 'invoice' }],
  actions: [{ kind: 'move', mailbox: 'Work/Invoices' }, { kind: 'read' }],
}
const lists: Rule = {
  name: 'Lists',
  match: 'any',
  conditions: [{ field: 'to', text: 'dev@lists.example' }, { field: 'subject', text: '[dev]' }],
  actions: [{ kind: 'label', mailbox: 'Lists' }, { kind: 'forward', address: 'archive@example.org' }],
}
const junk: Rule = { name: 'No "offers"', match: 'all', conditions: [{ field: 'subject', text: 'special \\ "offer"' }], actions: [{ kind: 'delete' }] }

suite('generate', () => {
  it('writes the script of the rules', () => {
    // The same file is run by the Go interpreter (internal/sieve), so
    // what the page writes and what the server runs cannot drift apart.
    expect(generate([invoices, lists, junk])).toBe(readFileSync('projects/jmap/src/lib/sieve/testdata/rules.sieve', 'utf8'))
  })

  it('requires only what the rules use', () => {
    expect(generate([junk])).not.toContain('require')
    expect(generate([{ ...junk, actions: [{ kind: 'read' }] }])).toContain('require ["imap4flags"];')
    expect(generate([{ ...junk, actions: [{ kind: 'forward', address: 'a@b.example' }] }])).toContain('require ["copy"];')
    expect(generate([])).toBe(`${MARK}\n${generate([]).split('\n').slice(1, 3).join('\n')}\n`)
  })

  it('quotes what could end a string', () => {
    expect(quote('a "b" \\ c')).toBe('"a \\"b\\" \\\\ c"')
  })
})

suite('parse', () => {
  it('reads back what generate wrote', () => {
    for (const rules of [[], [invoices], [lists, junk], [invoices, lists, junk]]) {
      expect(parse(generate(rules))).toEqual(rules.map((r) => ({ ...r, actions: [...r.actions].sort((a, b) => order(a.kind) - order(b.kind)) })))
    }
  })

  it('takes CRLF line ends, which is how a script is stored', () => {
    expect(parse(generate([invoices]).replace(/\n/g, '\r\n'))).toHaveLength(1)
  })

  it('is null for a script that is not the page\'s own', () => {
    const own = generate([invoices, lists])
    for (const other of [
      '',
      'keep;\n',
      'require "fileinto";\nfileinto "x";\n',
      own.replace(MARK, '# robinmail rules 2'),
      own + 'keep;\n', // something added by hand
      own.replace('    stop;\n', ''), // something taken away
      own.replace(':contains "from"', ':is "from"'), // another test
      own.replace('fileinto :create "Work/Invoices";', 'fileinto :create "Work/Invoices"; keep;'),
      own.replace('if allof', 'if  allof'),
      own.replace('"billing@"', '"billing@" '),
      own.replace('# rule: Lists', '# Lists'),
    ]) {
      expect(parse(other)).toBeNull()
    }
  })
})

function order(kind: string): number {
  return ['read', 'label', 'forward', 'move', 'delete'].indexOf(kind)
}

suite('problem', () => {
  it('lets a good rule through and names what is wrong with a bad one', () => {
    expect(problem(invoices)).toBe('')
    expect(problem(lists)).toBe('')
    expect(problem({ ...invoices, name: ' ' })).toContain('name')
    expect(problem({ ...invoices, name: 'a\nb' })).toContain('one line')
    expect(problem({ ...invoices, conditions: [] })).toContain('look for')
    expect(problem({ ...invoices, conditions: [{ field: 'from', text: '' }] })).toContain('look for')
    expect(problem({ ...invoices, conditions: [{ field: 'from', text: 'a"\r\n; discard; #' }] })).toContain('one line')
    expect(problem({ ...invoices, actions: [] })).toContain('what to do')
    expect(problem({ ...invoices, actions: [{ kind: 'move', mailbox: '' }] })).toContain('mailbox')
    expect(problem({ ...invoices, actions: [{ kind: 'forward', address: 'not an address' }] })).toContain('address')
    expect(problem({ ...invoices, actions: [{ kind: 'forward', address: 'a@b.example, c@d.example' }] })).toContain('address')
    expect(problem({ ...invoices, actions: [{ kind: 'move', mailbox: 'x' }, { kind: 'delete' }] })).toContain('moved and deleted')
    expect(problem({ ...invoices, actions: [{ kind: 'read' }, { kind: 'read' }] })).toContain('once')
  })
})

suite('describe', () => {
  it('says what a rule does', () => {
    expect(describe(invoices)).toEqual({
      when: 'From contains “billing@” and Subject contains “invoice”',
      then: 'mark as read, move to Work/Invoices',
    })
    expect(describe(lists).when).toBe('To or Cc contains “dev@lists.example” or Subject contains “[dev]”')
  })
})
