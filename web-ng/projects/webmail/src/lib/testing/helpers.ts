// For the tests of this library only (nothing here is exported or built).

import type { ComponentFixture } from '@angular/core/testing'

// until waits for something a test expects to become true: the page is
// drawn a moment after the state changes.
export async function until(what: () => boolean, fixture?: ComponentFixture<unknown>, ms = 2000): Promise<void> {
  const end = Date.now() + ms
  for (;;) {
    fixture?.detectChanges()
    if (what()) return
    if (Date.now() > end) throw new Error(`timed out waiting for: ${what}`)
    await new Promise((r) => setTimeout(r, 5))
  }
}

export function text(el: Element | null | undefined): string {
  return (el?.textContent ?? '').replace(/\s+/g, ' ').trim()
}

export function type(input: HTMLInputElement | HTMLTextAreaElement, value: string): void {
  input.value = value
  input.dispatchEvent(new Event('input', { bubbles: true }))
}
