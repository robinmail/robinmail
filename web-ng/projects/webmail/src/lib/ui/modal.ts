import { Dialog, type DialogRef } from '@angular/cdk/dialog'
import { ChangeDetectionStrategy, Component, type OnDestroy, type TemplateRef, effect, inject, input, model, viewChild } from '@angular/core'
import { Icon } from './icon'

// A dialog over the page (CDK Dialog: focus trap, Escape, role="dialog").
// The content is what the element holds; [open] says whether it shows.
@Component({
  selector: 'rm-modal',
  imports: [Icon],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <ng-template #tpl>
      <div class="flex max-h-[90vh] w-[calc(100vw-2rem)] flex-col rounded-lg bg-default shadow-xl ring ring-default" [class]="wide() ? 'max-w-2xl' : 'max-w-lg'">
        <div class="flex items-start gap-2 border-b border-default p-4">
          <div class="min-w-0 flex-1">
            <h2 class="font-semibold text-highlighted break-words">{{ title() }}</h2>
            @if (description()) {
              <p class="mt-1 text-sm text-muted">{{ description() }}</p>
            }
          </div>
          <button type="button" class="rounded p-1 text-dimmed hover:text-highlighted" aria-label="Close" (click)="open.set(false)">
            <rm-icon name="x" class="size-4" />
          </button>
        </div>
        <div class="overflow-y-auto p-4"><ng-content /></div>
        <div class="flex justify-end gap-2 border-t border-default p-4 empty:hidden"><ng-content select="[modal-footer]" /></div>
      </div>
    </ng-template>
  `,
})
export class Modal implements OnDestroy {
  readonly open = model(false)
  readonly title = input('')
  readonly description = input('')
  readonly wide = input(false)
  private readonly tpl = viewChild.required<TemplateRef<unknown>>('tpl')
  private readonly dialog = inject(Dialog)
  private ref: DialogRef<unknown> | null = null

  constructor() {
    effect(() => {
      const open = this.open()
      if (open && !this.ref) {
        const ref = this.dialog.open(this.tpl(), { ariaLabel: this.title(), autoFocus: 'first-tabbable', restoreFocus: true })
        this.ref = ref
        ref.closed.subscribe(() => {
          if (this.ref === ref) this.ref = null
          this.open.set(false)
        })
      } else if (!open && this.ref) {
        const ref = this.ref
        this.ref = null
        ref.close()
      }
    })
  }

  ngOnDestroy(): void {
    this.ref?.close()
  }
}
