import { computed } from '@angular/core'
import { describe, expect, it } from 'vitest'
import { reactive, toRaw } from './reactive'

describe('reactive', () => {
  it('lets a computed follow nested writes, pushes and deletes', () => {
    const s = reactive({ list: { ids: ['a'] as string[], total: 1 }, byId: {} as Record<string, { n: number }> })
    const sum = computed(() => s.list.ids.join(',') + '/' + s.list.total + '/' + Object.keys(s.byId).join(','))
    expect(sum()).toBe('a/1/')
    s.list.ids.push('b')
    s.list.total = 2
    s.byId['x'] = { n: 1 }
    expect(sum()).toBe('a,b/2/x')
    delete s.byId['x']
    s.list.ids = []
    expect(sum()).toBe('/2/')
  })

  it('gives the same proxy for the same object and stores raw values', () => {
    const s = reactive({ a: { n: 1 }, list: [] as { n: number }[] })
    expect(s.a).toBe(s.a)
    s.list.push(s.a)
    expect(s.list.includes(s.a)).toBe(true)
    expect(toRaw(s).list[0]).toBe(toRaw(s.a))
    const raw = { n: 2 }
    s.list.push(raw)
    expect(s.list.indexOf(raw)).toBe(1)
    expect(s.list.indexOf(s.list[1]!)).toBe(1)
    expect(s.list.includes(raw)).toBe(true)
  })

  it('leaves Maps, Sets and class instances alone', () => {
    const set = new Set([1])
    const s = reactive({ set })
    expect(s.set).toBe(set)
  })
})
