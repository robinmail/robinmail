import { describe, expect, it } from 'vitest'
import { formatAddress, isValidEmail, parseAddresses, parseMailto } from './address'

describe('isValidEmail', () => {
  it.each(['a@b.example', 'ann.smith+tag@mail.example.org', "o'neil@example.org", 'postmaster@localhost', 'jörg@bücher.example'])('accepts %s', (s) => {
    expect(isValidEmail(s)).toBe(true)
  })
  it.each(['', 'ann', 'ann@', '@example.org', 'a b@example.org', 'ann@@example.org', '.ann@example.org', 'ann..b@example.org', 'ann@-example.org', 'ann@example..org', 'ann@exa mple.org', `${'a'.repeat(65)}@example.org`])(
    'refuses %j', (s) => {
      expect(isValidEmail(s)).toBe(false)
    })
})

describe('parseAddresses', () => {
  it('reads a name and an address', () => {
    expect(parseAddresses('Ann Smith <ann@example.org>')).toEqual({ addresses: [{ name: 'Ann Smith', email: 'ann@example.org' }], invalid: [] })
  })

  it('reads a pasted list with commas, semicolons and line ends', () => {
    const r = parseAddresses('ann@example.org, Bob <bob@example.org>;\ncarol@example.org\r\n<dave@example.org>')
    expect(r.addresses.map((a) => a.email)).toEqual(['ann@example.org', 'bob@example.org', 'carol@example.org', 'dave@example.org'])
    expect(r.addresses[1]!.name).toBe('Bob')
    expect(r.invalid).toEqual([])
  })

  it('keeps a comma inside a quoted name', () => {
    expect(parseAddresses('"Smith, Ann" <ann@example.org>, bob@example.org').addresses).toEqual([
      { name: 'Smith, Ann', email: 'ann@example.org' },
      { name: null, email: 'bob@example.org' },
    ])
  })

  it('unescapes a quoted name', () => {
    expect(parseAddresses('"Ann \\"the boss\\" Smith" <ann@example.org>').addresses[0]!.name).toBe('Ann "the boss" Smith')
  })

  it('reads bare addresses separated by spaces', () => {
    expect(parseAddresses('ann@example.org bob@example.org').addresses.map((a) => a.email)).toEqual(['ann@example.org', 'bob@example.org'])
  })

  it('reports what is not an address and keeps the rest', () => {
    const r = parseAddresses('ann@example.org, not an address, Bob <bob@>')
    expect(r.addresses.map((a) => a.email)).toEqual(['ann@example.org'])
    expect(r.invalid).toEqual(['not an address', 'Bob <bob@>'])
  })

  it('drops an address given twice, whatever its case', () => {
    expect(parseAddresses('ann@example.org, Ann <ANN@example.org>').addresses).toEqual([{ name: null, email: 'ann@example.org' }])
  })

  it('takes a mailto: prefix off', () => {
    expect(parseAddresses('mailto:ann@example.org').addresses).toEqual([{ name: null, email: 'ann@example.org' }])
  })

  it('gives nothing for nothing', () => {
    expect(parseAddresses('  ,; ')).toEqual({ addresses: [], invalid: [] })
  })
})

describe('formatAddress', () => {
  it('quotes a name that needs it and parses back', () => {
    const a = { name: 'Smith, Ann "boss"', email: 'ann@example.org' }
    expect(formatAddress(a)).toBe('"Smith, Ann \\"boss\\"" <ann@example.org>')
    expect(parseAddresses(formatAddress(a)).addresses).toEqual([a])
    expect(formatAddress({ name: null, email: 'ann@example.org' })).toBe('ann@example.org')
    expect(formatAddress({ name: 'Ann Smith', email: 'ann@example.org' })).toBe('Ann Smith <ann@example.org>')
  })
})

describe('parseMailto (RFC 6068)', () => {
  it('reads the address', () => {
    expect(parseMailto('mailto:ann@example.org')).toEqual({ to: [{ name: null, email: 'ann@example.org' }], cc: [], bcc: [], subject: '', body: '' })
  })

  it('reads several addresses and the fields', () => {
    const m = parseMailto('mailto:ann@example.org,bob@example.org?cc=carol@example.org&subject=Hello%20there&body=Line%20one%0D%0ALine%20two&to=dave@example.org&bcc=erin@example.org')!
    expect(m.to.map((a) => a.email)).toEqual(['ann@example.org', 'bob@example.org', 'dave@example.org'])
    expect(m.cc.map((a) => a.email)).toEqual(['carol@example.org'])
    expect(m.bcc.map((a) => a.email)).toEqual(['erin@example.org'])
    expect(m.subject).toBe('Hello there')
    expect(m.body).toBe('Line one\nLine two')
  })

  it('reads fields without an address and ignores unknown fields', () => {
    const m = parseMailto('MAILTO:?subject=x&in-reply-to=%3Cid%40example%3E&attach=/etc/passwd')!
    expect(m.to).toEqual([])
    expect(m.subject).toBe('x')
  })

  it('survives a broken escape and is not for other URLs', () => {
    expect(parseMailto('mailto:ann@example.org?subject=100%')!.subject).toBe('100%')
    expect(parseMailto('https://example.org')).toBeNull()
  })
})
