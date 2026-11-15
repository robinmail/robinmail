// What the settings pages share: the bar with the way back to the mail and
// the pages as links.

import { ChangeDetectionStrategy, Component, input } from '@angular/core'
import { RouterLink, RouterLinkActive } from '@angular/router'
import { Icon } from '../ui/icon'

const tabs = [
  { link: '/settings', label: 'Identities', icon: 'at-sign' },
  { link: '/settings/filters', label: 'Filters', icon: 'list-filter' },
  { link: '/settings/vacation', label: 'Vacation', icon: 'palmtree' },
  { link: '/settings/security', label: 'Security', icon: 'shield-check' },
]

@Component({
  selector: 'rm-settings-shell',
  imports: [RouterLink, RouterLinkActive, Icon],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'flex min-h-full flex-col bg-default' },
  template: `
    <header class="flex h-(--ui-header-height) shrink-0 items-center gap-2 border-b border-default px-3 sm:px-6">
      <a routerLink="/mail" class="inline-flex items-center rounded-md p-1.5 text-default hover:bg-elevated focus-visible:outline-2 focus-visible:outline-primary" aria-label="Back to the mail" title="Back to the mail">
        <rm-icon name="arrow-left" class="size-4" />
      </a>
      <h1 class="font-semibold text-highlighted">{{ title() }}</h1>
      <nav class="ml-auto flex items-center gap-1" aria-label="Settings">
        @for (t of tabs; track t.link) {
          <a
            [routerLink]="t.link" routerLinkActive="bg-elevated" [routerLinkActiveOptions]="{ exact: true }" ariaCurrentWhenActive="page" [attr.aria-label]="t.label"
            class="inline-flex items-center gap-1.5 rounded-md px-2.5 py-1.5 text-xs font-medium text-default hover:bg-elevated focus-visible:outline-2 focus-visible:outline-primary"
          >
            <rm-icon [name]="t.icon" class="size-4" />
            <!-- Four tabs do not fit a phone with their words. -->
            <span class="hidden sm:inline">{{ t.label }}</span>
          </a>
        }
      </nav>
    </header>
    <main class="mx-auto flex w-full flex-1 flex-col gap-4 p-4 sm:p-6" [class]="wide() ? 'max-w-3xl' : 'max-w-2xl'">
      <ng-content />
    </main>
  `,
})
export class SettingsShell {
  readonly title = input.required<string>()
  readonly wide = input(false)
  protected readonly tabs = tabs
}
