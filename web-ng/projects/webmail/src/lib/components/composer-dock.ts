// Where the open composers sit: along the bottom right of the window, or
// one at a time over the whole screen on a phone. It also owns what goes
// with writing mail wherever the user is in the app: the "undo" notice
// after a send, mailto: links, and the warning before the tab closes
// with something unsent.

import { ChangeDetectionStrategy, Component, type OnDestroy, computed, effect, inject, signal, untracked } from '@angular/core'
import { type Sent, parseMailto } from '@robinmail/jmap'
import { compose } from '../app'
import { Toaster } from '../ui/toast'
import { ComposerPanel } from './composer-panel'

// How many composers fit next to each other.
const PANEL = 36 * 16 + 12

@Component({
  selector: 'rm-composer-dock',
  imports: [ComposerPanel],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (compose.state.composers.length > 0) {
      <div
        data-testid="composer-dock"
        class="pointer-events-none fixed inset-x-0 bottom-0 z-20 flex flex-col-reverse items-stretch sm:left-auto sm:right-4 sm:flex-row-reverse sm:items-end sm:gap-3"
      >
        @for (c of compose.state.composers; track c.key) {
          <rm-composer-panel [composer]="c" [phone]="phone()" />
        }
      </div>
    }
  `,
})
export class ComposerDock implements OnDestroy {
  private readonly toaster = inject(Toaster)
  protected readonly compose = compose
  private readonly width = signal(window.innerWidth)
  protected readonly phone = computed(() => this.width() < 640)
  // A send removes its composer, so the notice is shown from here.
  private readonly noticed = new Set<number>()
  private before: number[] = []

  private readonly onResize = () => this.width.set(window.innerWidth)
  private readonly beforeUnload = (e: BeforeUnloadEvent) => {
    if (!compose.hasUnsaved()) return
    e.preventDefault()
    e.returnValue = ''
  }
  // mailto: links anywhere in the app open a composer.
  private readonly onClick = (e: MouseEvent) => {
    if (e.defaultPrevented || e.button !== 0) return
    const a = (e.target as Element | null)?.closest?.('a[href]')
    const m = a ? parseMailto(a.getAttribute('href') ?? '') : null
    if (!m) return
    e.preventDefault()
    compose.compose(m)
  }
  // The page is put away (tab switch, phone lock): save now, it may not come back.
  private readonly onHide = () => {
    if (document.visibilityState !== 'hidden') return
    for (const c of compose.state.composers) if (c.version !== c.savedVersion) void compose.save(c)
  }

  constructor() {
    try {
      compose.rich = localStorage.getItem('robinmail-rich') === '1'
    } catch {
      // Storage is off: plain text.
    }
    compose.onError = (message) => this.toaster.error(message)

    // When one more opens than fit, the ones opened longest ago are minimised.
    const open = computed(() => compose.state.composers.filter((c) => !c.minimized).map((c) => c.key).join(' '))
    effect(() => {
      const keys = open().split(' ').filter(Boolean).map(Number)
      const room = this.phone() ? 1 : Math.max(1, Math.floor((this.width() - 32) / PANEL))
      untracked(() => {
        const fresh = keys.filter((k) => !this.before.includes(k))
        let over = keys.length - room
        for (const c of compose.state.composers) {
          if (over <= 0) break
          if (c.minimized || fresh.includes(c.key)) continue
          c.minimized = true
          over--
        }
        this.before = compose.state.composers.filter((c) => !c.minimized).map((c) => c.key)
      })
    })

    const sent = computed(() => compose.state.sent.length)
    effect(() => {
      sent()
      untracked(() => {
        for (const s of compose.state.sent) {
          if (this.noticed.has(s.key)) continue
          this.noticed.add(s.key)
          this.onSent(s)
        }
      })
    })

    window.addEventListener('resize', this.onResize)
    window.addEventListener('beforeunload', this.beforeUnload)
    document.addEventListener('click', this.onClick)
    document.addEventListener('visibilitychange', this.onHide)
  }

  ngOnDestroy(): void {
    window.removeEventListener('resize', this.onResize)
    window.removeEventListener('beforeunload', this.beforeUnload)
    document.removeEventListener('click', this.onClick)
    document.removeEventListener('visibilitychange', this.onHide)
  }

  private onSent(s: Sent): void {
    const ms = Math.min(30_000, Math.max(3000, s.until - Date.now()))
    this.toaster.add({
      title: 'Message sent', description: `You can take it back for ${Math.round(ms / 1000)} seconds.`, icon: 'send', duration: ms,
      actions: [{ label: 'Undo', run: () => void this.undo(s) }],
    })
    setTimeout(() => {
      compose.forget(s)
      this.noticed.delete(s.key)
    }, ms + 1000)
  }

  private async undo(s: Sent): Promise<void> {
    try {
      await compose.undo(s)
      this.toaster.add({ title: 'The message was not sent. It is a draft again.', icon: 'undo-2' })
    } catch (e) {
      this.toaster.error(e instanceof Error ? e.message : String(e))
    }
  }
}
