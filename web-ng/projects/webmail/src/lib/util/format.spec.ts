import { describe, expect, it } from 'vitest'
import { octets } from './format'

describe('octets', () => {
  it('writes a size as a person reads it', () => {
    expect(octets(0)).toBe('0 B')
    expect(octets(1023)).toBe('1023 B')
    expect(octets(1024)).toBe('1 KB')
    expect(octets(1536)).toBe('1.5 KB')
    expect(octets(5 * 1024 * 1024)).toBe('5 MB')
    expect(octets(123.4 * 1024 * 1024)).toBe('123 MB')
    expect(octets(2.5 * 1024 ** 3)).toBe('2.5 GB')
    expect(octets(3 * 1024 ** 5)).toBe('3072 TB')
  })
})
