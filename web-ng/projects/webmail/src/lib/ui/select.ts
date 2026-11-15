import { CdkListbox, CdkOption } from '@angular/cdk/listbox'
import { CdkConnectedOverlay, CdkOverlayOrigin } from '@angular/cdk/overlay'
import { ChangeDetectionStrategy, Component, type ElementRef, computed, input, output, signal, viewChild } from '@angular/core'
import { Icon } from './icon'

export interface SelectOption {
  label: string
  value: string
}

// A choice of one out of a list: a button that opens a listbox (CDK
// Listbox: arrow keys, typing ahead, role="option"). Used where the list
// should look like the rest of the page; a plain <select> does elsewhere.
@Component({
  selector: 'rm-select',
  imports: [CdkConnectedOverlay, CdkOverlayOrigin, CdkListbox, CdkOption, Icon],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'block min-w-0' },
  template: `
    <button
      #button type="button" [id]="buttonId()" [attr.data-testid]="testid()" [attr.aria-label]="label() || null" aria-haspopup="listbox" [attr.aria-expanded]="open()"
      cdkOverlayOrigin #origin="cdkOverlayOrigin" (click)="open.set(!open())"
      class="flex w-full min-w-0 items-center gap-2 rounded-md text-left text-sm text-highlighted outline-none focus-visible:ring-2 focus-visible:ring-primary"
      [class]="plain() ? 'bg-transparent py-1' : 'bg-default px-2.5 py-1.5 ring ring-inset ring-accented'"
    >
      <span class="min-w-0 flex-1 truncate" [class.text-dimmed]="!current()">{{ current()?.label ?? placeholder() }}</span>
      <rm-icon name="chevron-down" class="size-4 text-dimmed" />
    </button>
    <ng-template
      cdkConnectedOverlay [cdkConnectedOverlayOrigin]="origin" [cdkConnectedOverlayOpen]="open()" [cdkConnectedOverlayHasBackdrop]="true"
      cdkConnectedOverlayBackdropClass="cdk-overlay-transparent-backdrop" [cdkConnectedOverlayMinWidth]="width()"
      (backdropClick)="close()" (detach)="open.set(false)" (overlayKeydown)="$event.key === 'Escape' && close()" (attach)="opened()"
    >
      <ul
        #list cdkListbox [cdkListboxValue]="chosen()" (cdkListboxValueChange)="pick($event.value[0])" [attr.aria-label]="label() || null"
        (keydown.enter)="later()" (keydown.space)="later()"
        class="my-1 flex max-h-72 flex-col overflow-y-auto rounded-md bg-default p-1 text-sm shadow-lg ring ring-default outline-none"
      >
        @for (o of options(); track o.value) {
          <li
            [cdkOption]="o.value" (click)="later()" class="flex cursor-pointer items-center gap-2 rounded px-2 py-1.5 text-default outline-none hover:bg-elevated aria-selected:font-medium aria-selected:text-highlighted [&.cdk-option-active]:bg-elevated"
          >{{ o.label }}</li>
        }
      </ul>
    </ng-template>
  `,
})
export class Select {
  readonly value = input.required<string>()
  readonly options = input.required<SelectOption[]>()
  readonly label = input('')
  readonly placeholder = input('')
  readonly buttonId = input<string | null>(null)
  readonly testid = input<string | null>(null)
  // No border: for a field that sits in a row of its own.
  readonly plain = input(false)
  readonly valueChange = output<string>()

  protected readonly open = signal(false)
  protected readonly width = signal(0)
  protected readonly current = computed(() => this.options().find((o) => o.value === this.value()))
  // What the listbox marks: nothing while the value is none of the options.
  protected readonly chosen = computed(() => (this.current() ? [this.value()] : []))
  private readonly button = viewChild.required<ElementRef<HTMLButtonElement>>('button')
  private readonly list = viewChild<ElementRef<HTMLElement>>('list')

  protected opened(): void {
    this.width.set(this.button().nativeElement.offsetWidth)
    setTimeout(() => this.list()?.nativeElement.focus())
  }

  protected close(): void {
    this.open.set(false)
    this.button().nativeElement.focus()
  }

  // The listbox tells of a change of the choice.
  protected pick(v: unknown): void {
    if (typeof v === 'string' && v !== this.value()) this.valueChange.emit(v)
  }

  // A click or Enter on an option closes the list, also on the one that
  // was chosen already, of which the listbox tells nothing. A moment
  // later: the listbox handles the same event first.
  protected later(): void {
    setTimeout(() => this.open() && this.close())
  }
}
