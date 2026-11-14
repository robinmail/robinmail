import { ChangeDetectionStrategy, Component, Injectable, inject, signal } from '@angular/core'
import { Icon } from './icon'

const KEY = 'robinmail-color-mode'

// Light or dark: what the user chose here, else what the system says. The
// choice is the class "dark" on <html> and one word in localStorage.
@Injectable({ providedIn: 'root' })
export class ColorMode {
  readonly dark = signal(false)

  constructor() {
    let stored: string | null = null
    try {
      stored = localStorage.getItem(KEY)
    } catch {
      // no storage: follow the system
    }
    const system = typeof matchMedia === 'function' && matchMedia('(prefers-color-scheme: dark)').matches
    this.apply(stored ? stored === 'dark' : system)
  }

  toggle(): void {
    this.apply(!this.dark())
    try {
      localStorage.setItem(KEY, this.dark() ? 'dark' : 'light')
    } catch {
      // it holds until the page is closed
    }
  }

  private apply(dark: boolean): void {
    this.dark.set(dark)
    document.documentElement.classList.toggle('dark', dark)
  }
}

@Component({
  selector: 'rm-color-mode-button',
  imports: [Icon],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <button
      type="button" class="inline-flex items-center rounded-md p-1.5 text-default hover:bg-elevated focus-visible:outline-2 focus-visible:outline-primary"
      [attr.aria-label]="mode.dark() ? 'Switch to light mode' : 'Switch to dark mode'" [title]="mode.dark() ? 'Switch to light mode' : 'Switch to dark mode'"
      (click)="mode.toggle()"
    >
      <rm-icon [name]="mode.dark() ? 'moon' : 'sun'" class="size-4" />
    </button>
  `,
})
export class ColorModeButton {
  protected readonly mode = inject(ColorMode)
}
