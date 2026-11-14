import { ChangeDetectionStrategy, Component } from '@angular/core'
import { RouterOutlet } from '@angular/router'
import { Toasts } from './ui/toast'

// What is around every screen: the notices.
@Component({
  selector: 'rm-webmail',
  imports: [RouterOutlet, Toasts],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'block h-full' },
  template: `
    <router-outlet />
    <rm-toasts />
  `,
})
export class Frame {}
