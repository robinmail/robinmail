// A field of addresses: each one a chip, typed or pasted text read as a
// list. What is not an address stays in the field as text and is marked.

import { ChangeDetectionStrategy, Component, type ElementRef, input, output, signal, viewChild } from '@angular/core'
import { type EmailAddress, dedupe, isValidEmail, parseAddresses } from '@robinmail/jmap'
import { Icon } from '../ui/icon'

@Component({
  selector: 'rm-address-input',
  imports: [Icon],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'flex min-w-0 flex-1 flex-col' },
  template: `
    <div class="flex min-w-0 flex-wrap items-center gap-1 py-1" (click)="focus()">
      @for (a of value(); track a.email; let i = $index) {
        <span class="flex max-w-full items-center gap-1 rounded-full bg-elevated py-0.5 pl-2.5 pr-1 text-sm text-default" [title]="a.email" data-testid="address-chip">
          <span class="truncate">{{ a.name?.trim() || a.email }}</span>
          <button
            type="button" class="rounded-full p-0.5 text-dimmed hover:bg-accented hover:text-highlighted focus-visible:outline-2 focus-visible:outline-primary"
            [attr.aria-label]="'Remove ' + a.email" (click)="$event.stopPropagation(); remove(i)"
          >
            <rm-icon name="x" class="block size-3.5" />
          </button>
        </span>
      }
      <input
        #input [id]="inputId()" type="text" inputmode="email" autocomplete="off" autocapitalize="off" spellcheck="false" [value]="text()"
        [attr.aria-label]="label()" [attr.aria-invalid]="bad() ? 'true' : null" [attr.aria-describedby]="bad() ? inputId() + '-error' : null"
        class="min-w-32 flex-1 bg-transparent py-0.5 text-sm outline-none placeholder:text-dimmed" [class]="bad() ? 'text-error' : 'text-highlighted'"
        (keydown)="onKey($event)" (paste)="onPaste($event)" (blur)="commit()" (input)="text.set(input.value); onInput()"
      >
    </div>
    @if (bad()) {
      <p [id]="inputId() + '-error'" class="pb-1 text-xs text-error" role="alert">This is not an email address. Correct it or remove it.</p>
    }
  `,
})
export class AddressInput {
  readonly value = input.required<EmailAddress[]>()
  readonly label = input.required<string>()
  readonly inputId = input.required<string>()
  readonly valueChange = output<EmailAddress[]>()
  readonly invalid = output<boolean>()

  protected readonly text = signal('')
  protected readonly bad = signal(false)
  private readonly input = viewChild.required<ElementRef<HTMLInputElement>>('input')

  focus(): void {
    this.input().nativeElement.focus()
  }

  // The field is written here and not only through the template: text
  // that was typed and is taken away again in the same moment leaves the
  // template with the value it had, and it would not touch the field.
  private setText(v: string): void {
    this.text.set(v)
    this.input().nativeElement.value = v
  }

  // commit turns the typed text into chips. It answers whether all of it
  // was an address.
  commit(): boolean {
    if (this.text().trim() === '') {
      this.setText('')
      this.setBad(false)
      return true
    }
    const r = parseAddresses(this.text())
    if (r.addresses.length) this.valueChange.emit(dedupe([...this.value(), ...r.addresses]))
    this.setText(r.invalid.join(', '))
    this.setBad(r.invalid.length > 0)
    return r.invalid.length === 0
  }

  private setBad(v: boolean): void {
    if (this.bad() === v) return
    this.bad.set(v)
    this.invalid.emit(v)
  }

  protected remove(i: number): void {
    this.valueChange.emit(this.value().filter((_, at) => at !== i))
    this.focus()
  }

  protected onKey(e: KeyboardEvent): void {
    const text = this.text()
    if (e.key === 'Enter' || e.key === ',' || e.key === ';') {
      if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) return // send
      e.preventDefault()
      this.commit()
    } else if (e.key === ' ' && isValidEmail(text.trim())) {
      e.preventDefault()
      this.commit()
    } else if (e.key === 'Tab' && text.trim() !== '') {
      if (!this.commit()) e.preventDefault()
    } else if (e.key === 'Backspace' && text === '' && this.value().length > 0) {
      this.remove(this.value().length - 1)
    }
  }

  protected onPaste(e: ClipboardEvent): void {
    const pasted = e.clipboardData?.getData('text/plain') ?? ''
    if (pasted === '') return
    e.preventDefault()
    this.setText(this.text().trim() ? `${this.text()}, ${pasted}` : pasted)
    this.commit()
  }

  protected onInput(): void {
    if (this.bad() && this.text().trim() === '') this.setBad(false)
  }
}
