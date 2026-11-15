import { ChangeDetectionStrategy, Component } from '@angular/core'
import { RouterLink, RouterLinkActive } from '@angular/router'
import { Icon } from '../ui/icon'

// Mail, calendar and contacts: the three parts of the webmail.
@Component({
  selector: 'rm-app-switcher',
  imports: [RouterLink, RouterLinkActive, Icon],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <nav class="grid grid-cols-3 gap-1 rounded-md bg-elevated/60 p-1 text-xs" aria-label="Apps">
      @for (a of apps; track a.path) {
        <a [routerLink]="a.path" routerLinkActive="bg-default text-highlighted shadow-sm" #rla="routerLinkActive" [attr.aria-current]="rla.isActive ? 'page' : null"
          class="flex flex-col items-center gap-0.5 rounded px-1 py-1.5 text-muted hover:text-highlighted focus-visible:outline-2 focus-visible:outline-primary" [attr.data-testid]="'app-' + a.id">
          <rm-icon [name]="a.icon" class="size-4" />{{ a.label }}
        </a>
      }
    </nav>
  `,
})
export class AppSwitcher {
  protected readonly apps = [
    { id: 'mail', path: '/mail', icon: 'mail', label: 'Mail' },
    { id: 'calendar', path: '/calendar', icon: 'calendar', label: 'Calendar' },
    { id: 'contacts', path: '/contacts', icon: 'users', label: 'Contacts' },
  ]
}
