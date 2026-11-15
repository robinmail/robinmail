import { provideZonelessChangeDetection } from '@angular/core'
import { TestBed } from '@angular/core/testing'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ColorMode } from './color-mode'

const root = document.documentElement

function start(systemDark: boolean): ColorMode {
  vi.stubGlobal('matchMedia', (q: string) => ({ matches: systemDark && q.includes('dark') }))
  TestBed.configureTestingModule({ providers: [provideZonelessChangeDetection()] })
  return TestBed.inject(ColorMode)
}

describe('light and dark', () => {
  beforeEach(() => {
    localStorage.clear()
    root.classList.remove('dark', 'light')
  })
  afterEach(() => vi.unstubAllGlobals())

  it('follows the system until the user chooses', () => {
    const mode = start(true)
    expect(mode.dark()).toBe(true)
    expect([...root.classList]).toEqual(['dark'])
    expect(localStorage.getItem('robinmail-color-mode')).toBeNull()
  })

  it('keeps the choice, also against the system', () => {
    const mode = start(true)
    mode.toggle()
    expect(mode.dark()).toBe(false)
    // "light" holds the system's dark colours back.
    expect([...root.classList]).toEqual(['light'])
    expect(localStorage.getItem('robinmail-color-mode')).toBe('light')

    TestBed.resetTestingModule()
    expect(start(true).dark()).toBe(false)
  })

  it('goes dark on a light system when asked', () => {
    const mode = start(false)
    expect([...root.classList]).toEqual(['light'])
    mode.toggle()
    expect([...root.classList]).toEqual(['dark'])
    expect(localStorage.getItem('robinmail-color-mode')).toBe('dark')
  })
})
