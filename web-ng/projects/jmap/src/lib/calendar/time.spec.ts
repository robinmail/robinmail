import { describe, expect, it } from 'vitest'
import { addDuration, describeRule, formatDuration, layoutDay, localDateTime, monthGrid, parseDuration, parseLocal, startOfWeek } from './time'

describe('calendar time', () => {
  it('reads and writes LocalDateTime (RFC 8984 1.4.4)', () => {
    const d = parseLocal('2026-12-07T09:30:00')!
    expect(d.getHours()).toBe(9)
    expect(localDateTime(d)).toBe('2026-12-07T09:30:00')
    expect(parseLocal('2026-12-07')!.getDate()).toBe(7)
    expect(parseLocal('nonsense')).toBeNull()
  })

  it('reads Durations (RFC 8984 1.4.6) and writes them back', () => {
    expect(parseDuration('P1DT2H30M')).toEqual({ days: 1, ms: (2 * 60 + 30) * 60_000 })
    expect(parseDuration('P2W')).toEqual({ days: 14, ms: 0 })
    expect(parseDuration('P')).toBeNull()
    expect(parseDuration('PT')).toBeNull()
    const a = new Date(2026, 0, 1, 10, 0), b = new Date(2026, 0, 1, 11, 15)
    expect(formatDuration(a, b, false)).toBe('PT1H15M')
    expect(formatDuration(a, a, false)).toBe('PT0S')
    expect(formatDuration(new Date(2026, 0, 1), new Date(2026, 0, 3), true)).toBe('P2D')
    expect(addDuration(a, 'PT1H15M').getTime()).toBe(b.getTime())
  })

  it('weeks start on Monday and a month shows six of them', () => {
    expect(startOfWeek(new Date(2026, 11, 10)).getDay()).toBe(1)
    const grid = monthGrid(new Date(2026, 11, 15))
    expect(grid).toHaveLength(42)
    expect(grid[0]!.getDay()).toBe(1)
    expect(grid.some((d) => d.getMonth() === 11 && d.getDate() === 1)).toBe(true)
  })

  it('puts overlapping events side by side', () => {
    const day = new Date(2026, 0, 5)
    const at = (h: number, m = 0) => new Date(2026, 0, 5, h, m)
    const placed = layoutDay(day, [
      { start: at(9), end: at(10) },
      { start: at(9, 30), end: at(11) },
      { start: at(12), end: at(13) },
    ])
    expect(placed.map((p) => [p.top, p.column, p.columns])).toEqual([[540, 0, 2], [570, 1, 2], [720, 0, 1]])
  })

  it('says how an event repeats', () => {
    expect(describeRule(null)).toBe('Does not repeat')
    expect(describeRule({ frequency: 'weekly', byDay: [{ day: 'mo' }, { day: 'we' }] })).toBe('Every week on Monday, Wednesday')
    expect(describeRule({ frequency: 'daily', interval: 2, count: 5 })).toBe('Every 2 days, 5 times')
  })
})
