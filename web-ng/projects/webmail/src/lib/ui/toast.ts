import { ChangeDetectionStrategy, Component, Injectable, inject, signal } from '@angular/core'
import { Icon } from './icon'

export interface Toast {
  id: number
  title: string
  description?: string
  color: 'error' | 'neutral' | 'success'
  icon?: string
  // Buttons on the notice; a click closes it.
  actions?: { label: string; testid?: string; run: () => void }[]
  testid?: string
}

// Notices in the corner. They go away by themselves; an error stays longer.
@Injectable({ providedIn: 'root' })
export class Toaster {
  readonly list = signal<Toast[]>([])
  private next = 1

  add(t: Omit<Toast, 'id' | 'color'> & { color?: Toast['color']; duration?: number }): number {
    const id = this.next++
    this.list.update((l) => [...l, { color: 'neutral', ...t, id }])
    const ms = t.duration ?? (t.color === 'error' ? 8000 : 5000)
    if (ms > 0) setTimeout(() => this.remove(id), ms)
    return id
  }

  error(title: string): number {
    return this.add({ title, color: 'error', icon: 'circle-alert' })
  }

  remove(id: number): void {
    this.list.update((l) => l.filter((t) => t.id !== id))
  }
}

// Notices sit top right: the bottom right is where messages are written.
@Component({
  selector: 'rm-toasts',
  imports: [Icon],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="pointer-events-none fixed right-0 top-0 z-[1100] flex w-full max-w-sm flex-col gap-2 p-4" aria-live="polite">
      @for (t of toaster.list(); track t.id) {
        <div
          class="pointer-events-auto flex items-start gap-2 rounded-lg bg-default p-3 text-sm shadow-lg ring"
          [class]="t.color === 'error' ? 'ring-error/40' : 'ring-default'" role="status" [attr.data-testid]="t.testid ?? 'toast'"
        >
          @if (t.icon) {
            <rm-icon [name]="t.icon" class="mt-0.5 size-4" [class]="t.color === 'error' ? 'text-error' : t.color === 'success' ? 'text-success' : 'text-muted'" />
          }
          <div class="min-w-0 flex-1">
            <p class="font-medium text-highlighted break-words">{{ t.title }}</p>
            @if (t.description) {
              <p class="text-muted break-words">{{ t.description }}</p>
            }
            @if (t.actions?.length) {
              <div class="mt-2 flex gap-2">
                @for (a of t.actions; track a.label) {
                  <button
                    type="button" class="rounded-md bg-elevated px-2 py-1 text-xs font-medium ring ring-inset ring-accented hover:bg-accented/60"
                    [attr.data-testid]="a.testid" (click)="a.run(); toaster.remove(t.id)"
                  >{{ a.label }}</button>
                }
              </div>
            }
          </div>
          <button type="button" class="rounded p-0.5 text-dimmed hover:text-highlighted" aria-label="Close" (click)="toaster.remove(t.id)">
            <rm-icon name="x" class="size-4" />
          </button>
        </div>
      }
    </div>
  `,
})
export class Toasts {
  protected readonly toaster = inject(Toaster)
}
