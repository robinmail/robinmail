// A menu of mailboxes behind a button: "move" picks one; "labels" ticks
// the mailboxes the messages are in.

import { CdkMenu, CdkMenuItem, CdkMenuTrigger } from '@angular/cdk/menu'
import { ChangeDetectionStrategy, Component, computed, inject, input, output } from '@angular/core'
import type { Id } from '@robinmail/jmap'
import { MailView } from '../mail-view'
import { Btn, MENU, MENU_ITEM } from '../ui/controls'
import { Icon } from '../ui/icon'
import { mailboxIcon } from '../util/format'

@Component({
  selector: 'rm-move-menu',
  imports: [CdkMenu, CdkMenuItem, CdkMenuTrigger, Btn, Icon],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <button
      rmBtn type="button" variant="ghost" color="neutral" [size]="size()" [attr.aria-label]="label()" [title]="label()" [disabled]="disabled()"
      [attr.data-testid]="testid()" [cdkMenuTriggerFor]="menu"
    >
      <rm-icon [name]="icon()" class="size-4" />
    </button>
    <ng-template #menu>
      <div class="${MENU}" cdkMenu>
        <div class="px-2 py-1.5 text-xs font-semibold text-highlighted">{{ mode() === 'move' ? 'Move to' : 'Mailboxes of this conversation' }}</div>
        @for (o of options(); track o.mailbox.id) {
          @if (mode() === 'move') {
            <button type="button" class="${MENU_ITEM}" [class.pl-6]="o.depth > 0" cdkMenuItem (cdkMenuItemTriggered)="move.emit(o.mailbox.id)">
              <rm-icon [name]="roleIcon(o.mailbox.role)" class="size-4 text-dimmed" /><span class="truncate">{{ o.mailbox.name }}</span>
            </button>
          } @else {
            <!-- Not a CDK item: a click on one of those closes the menu, and several can be ticked here. -->
            <button
              type="button" class="${MENU_ITEM}" [class.pl-6]="o.depth > 0" role="menuitemcheckbox" [attr.aria-checked]="isChecked(o.mailbox.id)"
              (click)="labelled.emit({ mailbox: o.mailbox.id, on: !isChecked(o.mailbox.id) })"
            >
              <rm-icon [name]="roleIcon(o.mailbox.role)" class="size-4 text-dimmed" /><span class="flex-1 truncate">{{ o.mailbox.name }}</span>
              @if (isChecked(o.mailbox.id)) {
                <rm-icon name="check" class="size-4" />
              }
            </button>
          }
        }
      </div>
    </ng-template>
  `,
})
export class MoveMenu {
  private readonly view = inject(MailView)
  readonly mode = input.required<'move' | 'labels'>()
  // move: the mailbox that is left out. labels: not used.
  readonly exclude = input<Id>('')
  // labels: the mailboxes all of the messages are in.
  readonly checked = input<Id[]>([])
  readonly label = input.required<string>()
  readonly icon = input('folder-input')
  readonly size = input<'sm' | 'md'>('md')
  readonly disabled = input(false)
  readonly testid = input<string | null>(null)
  readonly move = output<Id>()
  readonly labelled = output<{ mailbox: Id; on: boolean }>()

  protected readonly options = computed(() => this.view.mailboxes().filter((o) => this.mode() === 'labels' || o.mailbox.id !== this.exclude()))
  protected isChecked(id: Id): boolean {
    return this.checked().includes(id)
  }
  protected roleIcon(role: string | null): string {
    return mailboxIcon(role)
  }
}
