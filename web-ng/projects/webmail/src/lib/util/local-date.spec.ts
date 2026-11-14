import { describe, expect, it } from 'vitest'
import { fromLocalInput, toLocalInput } from './local-date'

describe('local dates', () => {
  it('round-trips through the input value, whatever the time zone', () => {
    for (const utc of ['2026-12-24T08:00:00Z', '2027-01-01T00:00:00Z', '2026-03-29T01:30:00Z']) {
      expect(fromLocalInput(toLocalInput(utc))).toBe(utc)
    }
  })
  it('has no seconds and no zone in the input value', () => {
    expect(toLocalInput('2026-12-24T08:00:00Z')).toMatch(/^\d{4}-\d\d-\d\dT\d\d:\d\d$/)
  })
  it('is empty and null for nothing', () => {
    expect(toLocalInput(null)).toBe('')
    expect(toLocalInput('nonsense')).toBe('')
    expect(fromLocalInput('')).toBeNull()
    expect(fromLocalInput('nonsense')).toBeNull()
  })
})
