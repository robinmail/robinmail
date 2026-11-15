import { ChangeDetectionStrategy, Component, input, output } from '@angular/core'

// On or off, at once: a button with role="switch" and its label.
@Component({
  selector: 'rm-switch',
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'inline-flex' },
  template: `
    <button
      type="button" role="switch" [attr.aria-checked]="checked()" [disabled]="disabled()" [attr.data-testid]="testid()" (click)="checkedChange.emit(!checked())"
      class="group inline-flex items-center gap-2 rounded text-sm font-medium text-default outline-none focus-visible:ring-2 focus-visible:ring-primary disabled:cursor-not-allowed disabled:opacity-60"
    >
      <span class="relative inline-block h-5 w-9 shrink-0 rounded-full transition-colors" [class]="checked() ? 'bg-primary' : 'bg-accented'">
        <span class="absolute left-0.5 top-0.5 size-4 rounded-full bg-white shadow transition-transform" [class.translate-x-4]="checked()"></span>
      </span>
      {{ label() }}
    </button>
  `,
})
export class Switch {
  readonly checked = input.required<boolean>()
  readonly label = input.required<string>()
  readonly disabled = input(false)
  readonly testid = input<string | null>(null)
  readonly checkedChange = output<boolean>()
}
