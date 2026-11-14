import { ChangeDetectionStrategy, Component } from '@angular/core'
import { RouterOutlet } from '@angular/router'
import { auth } from './app'
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
export class Frame {
  protected readonly auth = auth
}
