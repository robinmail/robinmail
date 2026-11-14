// The filter of the list. It holds only what Email/query supports on this
// server: from, subject, unread, flagged, has attachment (PLAN.md, phase 3).

import { CdkConnectedOverlay, CdkOverlayOrigin } from '@angular/cdk/overlay'
import { ChangeDetectionStrategy, Component, computed, effect, inject, signal, untracked } from '@angular/core'
import { MailView } from '../mail-view'
import { Btn, Input, LABEL } from '../ui/controls'
import { Icon } from '../ui/icon'

@Component({
  selector: 'rm-filter-box',
  imports: [CdkConnectedOverlay, CdkOverlayOrigin, Btn, Input, Icon],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <span class="relative inline-flex" cdkOverlayOrigin #origin="cdkOverlayOrigin">
      <button rmBtn type="button" variant="ghost" color="neutral" aria-label="Filter messages" title="Filter messages" data-testid="filter" (click)="open.set(!open())">
        <rm-icon name="list-filter" class="size-4" />
      </button>
      @if (filterCount() > 0) {
        <span class="pointer-events-none absolute right-0 top-0 flex size-4 items-center justify-center rounded-full bg-primary text-[10px] font-semibold text-inverted">{{ filterCount() }}</span>
      }
    </span>
    <ng-template
      cdkConnectedOverlay [cdkConnectedOverlayOrigin]="origin" [cdkConnectedOverlayOpen]="open()" [cdkConnectedOverlayHasBackdrop]="true"
      cdkConnectedOverlayBackdropClass="cdk-overlay-transparent-backdrop" [cdkConnectedOverlayPositions]="positions"
      (backdropClick)="close()" (detach)="open.set(false)" (overlayKeydown)="$event.key === 'Escape' && close()"
    >
      <form class="mt-1 flex w-72 max-w-[calc(100vw-2rem)] flex-col gap-3 rounded-md bg-default p-4 shadow-lg ring ring-default" role="dialog" aria-label="Filter messages" (submit)="$event.preventDefault(); close()">
        <label class="${LABEL}">
          <span>From</span>
          <input rmInput #f name="from" placeholder="Name or address" autocomplete="off" autofocus [value]="from()" (input)="from.set(f.value); typed()">
        </label>
        <label class="${LABEL}">
          <span>Subject</span>
          <input rmInput #s name="subject" placeholder="Words in the subject" autocomplete="off" [value]="subject()" (input)="subject.set(s.value); typed()">
        </label>
        <label class="flex items-center gap-2 text-sm"><input #u type="checkbox" [checked]="view.spec().unread" (change)="view.setFilter({ unread: u.checked })">Unread</label>
        <label class="flex items-center gap-2 text-sm"><input #fl type="checkbox" [checked]="view.spec().flagged" (change)="view.setFilter({ flagged: fl.checked })">Flagged</label>
        <label class="flex items-center gap-2 text-sm"><input #a type="checkbox" [checked]="view.spec().attachment" (change)="view.setFilter({ attachment: a.checked })">Has attachment</label>
        <div class="flex justify-between gap-2 pt-1">
          <button rmBtn type="button" variant="ghost" color="neutral" size="sm" [disabled]="filterCount() === 0" (click)="clear()">Clear</button>
          <button rmBtn type="submit" size="sm">Done</button>
        </div>
      </form>
    </ng-template>
  `,
})
export class FilterBox {
  protected readonly view = inject(MailView)
  protected readonly open = signal(false)
  // The search text has its own box over the list and is not counted here.
  protected readonly filterCount = computed(() => {
    const s = this.view.spec()
    return [s.from, s.subject, s.unread, s.flagged, s.attachment].filter(Boolean).length
  })
  protected readonly from = signal(this.view.spec().from)
  protected readonly subject = signal(this.view.spec().subject)
  protected readonly positions = [
    { originX: 'end', originY: 'bottom', overlayX: 'end', overlayY: 'top' } as const,
    { originX: 'start', originY: 'bottom', overlayX: 'start', overlayY: 'top' } as const,
  ]
  // Typing changes the route a moment after the last key.
  private timer: ReturnType<typeof setTimeout> | undefined

  constructor() {
    effect(() => {
      const s = this.view.spec()
      untracked(() => {
        this.from.set(s.from)
        this.subject.set(s.subject)
      })
    })
  }

  protected typed(): void {
    clearTimeout(this.timer)
    this.timer = setTimeout(() => this.apply(), 350)
  }
  private apply(): void {
    clearTimeout(this.timer)
    const f = this.from().trim()
    const s = this.subject().trim()
    if (f !== this.view.spec().from || s !== this.view.spec().subject) void this.view.setFilter({ from: f, subject: s })
  }
  protected close(): void {
    this.apply()
    this.open.set(false)
  }
  protected clear(): void {
    clearTimeout(this.timer)
    void this.view.setFilter({ from: '', subject: '', unread: false, flagged: false, attachment: false })
  }
}
