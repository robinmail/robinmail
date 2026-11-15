// The search box over the list: full-text search in the open mailbox
// (Email/query with the text condition). Words must all be in a message;
// "quoted words" must be there as written.

import { ChangeDetectionStrategy, Component, type ElementRef, effect, inject, signal, untracked, viewChild } from '@angular/core'
import { MailView } from '../mail-view'
import { Icon } from '../ui/icon'

@Component({
  selector: 'rm-search-box',
  imports: [Icon],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <form role="search" class="border-b border-default px-3 py-2" (submit)="$event.preventDefault(); apply()">
      <div class="relative">
        <rm-icon name="search" class="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-dimmed" />
        <input
          #input name="q" type="search" autocomplete="off" data-testid="search" [value]="text()"
          class="w-full rounded-md border-0 bg-default py-1.5 pl-8 pr-8 text-sm text-highlighted ring ring-inset ring-accented placeholder:text-dimmed outline-none focus-visible:ring-2 focus-visible:ring-primary [&::-webkit-search-cancel-button]:hidden"
          [placeholder]="'Search ' + (view.mailbox()?.name ?? 'mail')" [attr.aria-label]="'Search ' + (view.mailbox()?.name ?? 'mail')"
          (input)="text.set(input.value); typed()" (keydown.escape)="clear()"
        >
        @if (text()) {
          <button
            type="button" class="absolute right-1.5 top-1/2 -translate-y-1/2 rounded p-1 text-muted hover:text-default" aria-label="Clear the search"
            data-testid="search-clear" (click)="clear()"
          >
            <rm-icon name="x" class="size-4" />
          </button>
        }
      </div>
    </form>
  `,
})
export class SearchBox {
  protected readonly view = inject(MailView)
  protected readonly text = signal(this.view.spec().text)
  private readonly input = viewChild.required<ElementRef<HTMLInputElement>>('input')
  // Typing searches a moment after the last key; Enter at once.
  private timer: ReturnType<typeof setTimeout> | undefined

  constructor() {
    effect(() => {
      const t = this.view.spec().text
      untracked(() => {
        if (t !== this.text().trim()) this.text.set(t)
      })
    })
  }

  protected typed(): void {
    clearTimeout(this.timer)
    this.timer = setTimeout(() => this.apply(), 400)
  }
  protected apply(): void {
    clearTimeout(this.timer)
    const t = this.text().trim()
    if (t !== this.view.spec().text) void this.view.setFilter({ text: t })
  }
  protected clear(): void {
    this.text.set('')
    // Also when the template still holds the empty value from before the typing.
    this.input().nativeElement.value = ''
    this.apply()
  }
  focus(): void {
    this.input().nativeElement.focus()
  }
}
