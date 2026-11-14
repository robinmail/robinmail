import { CdkMenu, CdkMenuItem, CdkMenuTrigger } from '@angular/cdk/menu'
import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core'
import { FormsModule } from '@angular/forms'
import { Router, RouterLink } from '@angular/router'
import type { Id, Mailbox } from '@robinmail/jmap'
import { store } from '../app'
import { MailView } from '../mail-view'
import { ALERT, Btn, Input, LABEL, MENU, MENU_ITEM } from '../ui/controls'
import { Icon } from '../ui/icon'
import { Modal } from '../ui/modal'
import { mailboxIcon } from '../util/format'

type Dialog = { kind: 'create'; parentId: Id | null } | { kind: 'rename'; mailbox: Mailbox } | { kind: 'delete'; mailbox: Mailbox }

@Component({
  selector: 'rm-mailbox-nav',
  imports: [RouterLink, FormsModule, CdkMenu, CdkMenuItem, CdkMenuTrigger, Btn, Input, Icon, Modal],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <nav class="flex flex-col gap-0.5" aria-label="Mailboxes">
      @for (o of view.mailboxes(); track o.mailbox.id) {
        <div class="group flex items-center rounded-md" [class]="o.mailbox.id === view.mailboxId() ? 'bg-elevated' : 'hover:bg-elevated/50'">
          <a
            [routerLink]="['/mail', o.mailbox.id]"
            class="flex min-w-0 flex-1 items-center gap-2 rounded-md py-1.5 pr-2 text-sm focus-visible:outline-2 focus-visible:outline-primary"
            [class]="o.mailbox.id === view.mailboxId() ? 'font-medium text-highlighted' : 'text-muted group-hover:text-highlighted'"
            [style.padding-left.rem]="0.625 + o.depth * 1.125"
            [attr.aria-current]="o.mailbox.id === view.mailboxId() ? 'page' : null"
            [attr.data-mailbox]="o.mailbox.role || o.mailbox.name"
          >
            <rm-icon [name]="icon(o.mailbox.role)" class="size-4" />
            <span class="flex-1 truncate">{{ o.mailbox.name }}</span>
            @if (o.mailbox.unreadEmails > 0) {
              <span class="text-xs font-semibold tabular-nums text-primary" data-unread [attr.aria-label]="o.mailbox.unreadEmails + ' unread'">{{ o.mailbox.unreadEmails }}</span>
            } @else if (o.mailbox.role === 'trash' && o.mailbox.totalEmails > 0) {
              <span class="text-xs tabular-nums text-dimmed" data-total>{{ o.mailbox.totalEmails }}</span>
            }
          </a>
          @if (!o.mailbox.role) {
            <button
              type="button" [attr.aria-label]="'Actions for ' + o.mailbox.name" [cdkMenuTriggerFor]="menu"
              class="mr-1 rounded p-1 text-default hover:bg-accented/60 focus-visible:opacity-100 aria-expanded:opacity-100 lg:opacity-0 lg:group-hover:opacity-100"
            >
              <rm-icon name="ellipsis" class="size-4" />
            </button>
            <ng-template #menu>
              <div class="${MENU}" cdkMenu>
                <button type="button" class="${MENU_ITEM}" cdkMenuItem (cdkMenuItemTriggered)="show({ kind: 'rename', mailbox: o.mailbox })"><rm-icon name="pencil" class="size-4 text-dimmed" />Rename</button>
                <button type="button" class="${MENU_ITEM}" cdkMenuItem (cdkMenuItemTriggered)="show({ kind: 'create', parentId: o.mailbox.id })"><rm-icon name="folder-plus" class="size-4 text-dimmed" />New mailbox inside</button>
                <div class="my-1 h-px bg-accented" role="separator"></div>
                <button type="button" class="${MENU_ITEM} !text-error" cdkMenuItem (cdkMenuItemTriggered)="show({ kind: 'delete', mailbox: o.mailbox })"><rm-icon name="trash-2" class="size-4" />Delete</button>
              </div>
            </ng-template>
          }
        </div>
      }
      <button rmBtn type="button" variant="ghost" color="neutral" size="sm" class="mt-2 justify-start !text-muted" (click)="show({ kind: 'create', parentId: null })">
        <rm-icon name="plus" class="size-4" />New mailbox
      </button>
    </nav>

    <rm-modal [open]="dialog() !== null" (openChange)="$event || dialog.set(null)" [title]="title()" [description]="dialog()?.kind === 'delete' ? 'This cannot be undone.' : 'Mailboxes work as labels: a message can be in several.'">
      @let d = dialog();
      <form id="mailbox-form" class="flex flex-col gap-4" (ngSubmit)="submit()">
        @if (error()) {
          <div class="${ALERT}" role="alert"><rm-icon name="circle-alert" class="mt-0.5 size-4" /><span>{{ error() }}</span></div>
        }
        @if (d?.kind === 'delete') {
          @if (d.mailbox.totalEmails > 0) {
            <p class="text-sm">
              Its {{ d.mailbox.totalEmails }} {{ d.mailbox.totalEmails === 1 ? 'message is' : 'messages are' }} deleted with it.
              A message that is also in another mailbox stays there.
            </p>
          } @else {
            <p class="text-sm">The mailbox is empty.</p>
          }
        } @else {
          <label class="${LABEL}">
            <span>Name</span>
            <input rmInput name="name" autofocus maxlength="255" autocomplete="off" [(ngModel)]="name">
          </label>
          @if (d?.kind === 'create') {
            <label class="${LABEL}">
              <span>Inside</span>
              <select rmInput name="parent" [(ngModel)]="parent">
                <option value="none">None (top level)</option>
                @for (o of view.mailboxes(); track o.mailbox.id) {
                  <option [value]="o.mailbox.id">{{ indent(o.depth) }}{{ o.mailbox.name }}</option>
                }
              </select>
            </label>
          }
        }
      </form>
      <ng-container modal-footer>
        <button rmBtn type="button" variant="ghost" color="neutral" (click)="dialog.set(null)">Cancel</button>
        <button rmBtn type="submit" form="mailbox-form" [color]="d?.kind === 'delete' ? 'error' : 'primary'" [disabled]="busy()">
          {{ d?.kind === 'delete' ? 'Delete' : d?.kind === 'rename' ? 'Rename' : 'Create' }}
        </button>
      </ng-container>
    </rm-modal>
  `,
})
export class MailboxNav {
  protected readonly view = inject(MailView)
  private readonly router = inject(Router)

  protected readonly dialog = signal<Dialog | null>(null)
  protected readonly name = signal('')
  protected readonly parent = signal('none')
  protected readonly busy = signal(false)
  protected readonly error = signal('')

  protected readonly title = computed(() => {
    const d = this.dialog()
    if (!d) return ''
    if (d.kind === 'create') return 'New mailbox'
    return d.kind === 'rename' ? `Rename “${d.mailbox.name}”` : `Delete “${d.mailbox.name}”?`
  })

  protected icon(role: string | null): string {
    return mailboxIcon(role)
  }
  protected indent(depth: number): string {
    return '  '.repeat(depth)
  }

  protected show(d: Dialog): void {
    this.dialog.set(d)
    this.error.set('')
    this.name.set(d.kind === 'rename' ? d.mailbox.name : '')
    this.parent.set(d.kind === 'create' && d.parentId ? d.parentId : 'none')
  }

  protected async submit(): Promise<void> {
    const d = this.dialog()
    if (!d || this.busy()) return
    const n = this.name().trim()
    if (d.kind !== 'delete' && !n) {
      this.error.set('Give the mailbox a name.')
      return
    }
    this.busy.set(true)
    this.error.set('')
    try {
      if (d.kind === 'create') {
        const id = await store.createMailbox(n, this.parent() === 'none' ? null : this.parent())
        if (id) void this.router.navigate(['/mail', id])
      } else if (d.kind === 'rename') {
        if (n !== d.mailbox.name) await store.renameMailbox(d.mailbox.id, n)
      } else {
        await store.destroyMailbox(d.mailbox.id, d.mailbox.totalEmails > 0)
      }
      this.dialog.set(null)
    } catch (e) {
      this.error.set(e instanceof Error ? e.message : String(e))
    } finally {
      this.busy.set(false)
    }
  }
}
