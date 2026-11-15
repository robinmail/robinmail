import { ChangeDetectionStrategy, Component, type OnDestroy, inject } from '@angular/core'
import { Router, RouterOutlet } from '@angular/router'
import { auth, compose } from './app'
import { ComposerDock } from './components/composer-dock'
import { Toasts } from './ui/toast'

// What is around every screen: the notices, and the messages being written.
@Component({
  selector: 'rm-webmail',
  imports: [RouterOutlet, Toasts, ComposerDock],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'block h-full' },
  template: `
    <router-outlet />
    @if (auth.account) {
      <rm-composer-dock />
    }
    <rm-toasts />
  `,
})
export class Frame implements OnDestroy {
  protected readonly auth = auth
  private readonly router = inject(Router)

  // "c" starts a new message on the mail page. The key is heard here and
  // not by the mail page, because the composers live here: it also works
  // in the moment between a click on a link to the mail and the page
  // being drawn. The other keys are the mail page's own.
  private readonly onKey = (e: KeyboardEvent) => {
    if (e.key !== 'c' || e.defaultPrevented || e.metaKey || e.ctrlKey || e.altKey) return
    const t = e.target as HTMLElement | null
    if (t && (t.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(t.tagName))) return
    if (document.querySelector('[role="dialog"], [role="menu"], [role="listbox"]')) return
    const going = this.router.getCurrentNavigation()?.extractedUrl
    const url = going ? this.router.serializeUrl(going) : this.router.url
    if (!auth.account || !compose.state.identitiesLoaded || !/^\/mail(\/|\?|$)/.test(url)) return
    e.preventDefault()
    compose.compose()
  }

  constructor() {
    window.addEventListener('keydown', this.onKey)
  }

  ngOnDestroy(): void {
    window.removeEventListener('keydown', this.onKey)
  }
}
