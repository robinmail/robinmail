// The messages of one thread. Opening a thread marks it read.

import { CdkMenu, CdkMenuItem, CdkMenuTrigger } from '@angular/cdk/menu'
import { ChangeDetectionStrategy, Component, computed, effect, inject, signal, untracked } from '@angular/core'
import type { DeliveryStatus, Email, Id, Kind } from '@robinmail/jmap'
import { client, compose, store } from '../app'
import { MailView, actions, buildRow } from '../mail-view'
import { Badge, Btn, MENU, MENU_ITEM, SKELETON } from '../ui/controls'
import { Icon } from '../ui/icon'
import { addressList, displayName, fullDate, shortDate } from '../util/format'
import { MessageBody } from './message-body'
import { MoveMenu } from './move-menu'

interface Delivery {
  address: string
  label: string
  detail: string
  color: 'success' | 'neutral' | 'warning' | 'error'
  icon: string
}

function delivery(address: string, d: DeliveryStatus, pending: boolean): Delivery {
  // "unknown" is what JMAP calls mail another server took over: sent, as far as anyone can know.
  if (d.delivered === 'yes') return { address, label: 'Delivered', detail: d.smtpReply, color: 'success', icon: 'check-check' }
  if (d.delivered === 'unknown') return { address, label: 'Sent', detail: d.smtpReply || 'The receiving server accepted it.', color: 'neutral', icon: 'check' }
  if (d.delivered === 'no') return { address, label: 'Failed', detail: d.smtpReply, color: 'error', icon: 'circle-x' }
  return pending
    ? { address, label: 'Sending', detail: 'It leaves in a few seconds.', color: 'neutral', icon: 'clock' }
    : { address, label: 'Queued', detail: d.smtpReply || 'Waiting for its turn.', color: d.smtpReply ? 'warning' : 'neutral', icon: 'clock' }
}

@Component({
  selector: 'rm-thread-view',
  imports: [CdkMenu, CdkMenuItem, CdkMenuTrigger, Btn, Badge, Icon, MessageBody, MoveMenu],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'flex min-h-0 flex-1 flex-col', 'data-testid': 'thread' },
  template: `
    @let r = row();
    <div class="flex h-(--ui-header-height) shrink-0 items-center gap-1 border-b border-default px-2 sm:px-4">
      <button rmBtn type="button" variant="ghost" color="neutral" aria-label="Back to the list" title="Back to the list (Esc)" class="lg:hidden" (click)="view.closeThread()">
        <rm-icon name="arrow-left" class="size-4" />
      </button>
      <button rmBtn type="button" variant="ghost" color="neutral" aria-label="Close the conversation" title="Close (Esc)" class="max-lg:hidden" (click)="view.closeThread()">
        <rm-icon name="x" class="size-4" />
      </button>
      <div class="ml-auto flex items-center gap-0.5" role="toolbar" aria-label="Conversation actions">
        <button rmBtn type="button" variant="ghost" color="neutral" aria-label="Reply" title="Reply (R)" [disabled]="!r" class="max-sm:hidden" (click)="respond('reply')">
          <rm-icon name="reply" class="size-4" />
        </button>
        <button rmBtn type="button" variant="ghost" color="neutral" aria-label="Mark unread" title="Mark unread (U)" [disabled]="!r" (click)="markUnread()">
          <rm-icon name="mail" class="size-4" />
        </button>
        <button
          rmBtn type="button" variant="ghost" color="neutral" [attr.aria-label]="r?.flagged ? 'Remove flag' : 'Flag'" [title]="(r?.flagged ? 'Remove flag' : 'Flag') + ' (S)'"
          [attr.aria-pressed]="r?.flagged ?? false" [disabled]="!r" [class]="r?.flagged ? '!text-warning [&_path]:fill-current' : ''" data-testid="thread-flag" (click)="flag()"
        >
          <rm-icon name="star" class="size-4" />
        </button>
        <rm-move-menu mode="labels" icon="tags" label="Mailboxes of this conversation" [checked]="checked()" [disabled]="!r" testid="thread-labels" (labelled)="label($event.mailbox, $event.on)" />
        <rm-move-menu mode="move" label="Move to" [exclude]="view.mailboxId()" [disabled]="!r" testid="thread-move" (move)="move($event)" />
        <button
          rmBtn type="button" variant="ghost" color="neutral" [attr.aria-label]="isTrash() ? 'Delete forever' : 'Delete'" [title]="(isTrash() ? 'Delete forever' : 'Delete') + ' (#)'"
          [disabled]="!r" data-testid="thread-delete" (click)="remove()"
        >
          <rm-icon name="trash-2" class="size-4" />
        </button>
        <span class="mx-1 h-5 w-px bg-accented" role="separator"></span>
        <button rmBtn type="button" variant="ghost" color="neutral" aria-label="Newer conversation" title="Newer (K)" (click)="view.step(-1)"><rm-icon name="chevron-up" class="size-4" /></button>
        <button rmBtn type="button" variant="ghost" color="neutral" aria-label="Older conversation" title="Older (J)" (click)="view.step(1)"><rm-icon name="chevron-down" class="size-4" /></button>
      </div>
    </div>

    <div class="min-h-0 flex-1 overflow-y-auto overscroll-contain">
      @if (error()) {
        <div class="flex flex-col items-center gap-3 p-8 pt-16 text-center">
          <rm-icon name="triangle-alert" class="size-8 text-error" />
          <p class="text-sm text-muted">{{ error() }}</p>
          <button rmBtn type="button" variant="subtle" color="neutral" (click)="view.closeThread()">Back to the list</button>
        </div>
      } @else if (loading()) {
        <div class="flex flex-col gap-3 p-6" aria-busy="true">
          <div class="h-7 w-2/3 ${SKELETON}"></div>
          <div class="h-4 w-1/3 ${SKELETON}"></div>
          <div class="mt-4 h-40 w-full ${SKELETON}"></div>
        </div>
      } @else {
        <div class="mx-auto flex max-w-4xl flex-col gap-3 p-3 sm:p-6">
          <h1 class="px-1 text-xl font-semibold text-highlighted break-words" data-testid="thread-subject">{{ subject() }}</h1>
          @for (e of emails(); track e.id) {
            @let open = expanded().has(e.id);
            <article [attr.data-email]="e.id" class="rounded-lg border border-default bg-default" [attr.aria-label]="'Message from ' + (addressList(e.from) || 'unknown')">
              <div class="flex items-start gap-2 px-3 py-2.5 sm:px-4">
                <button type="button" class="flex min-w-0 flex-1 flex-col rounded text-left focus-visible:outline-2 focus-visible:outline-primary" [attr.aria-expanded]="open" (click)="toggle(e.id)">
                  <span class="flex w-full items-baseline gap-2">
                    <span class="truncate text-sm font-semibold text-highlighted">{{ names(e.from) || '(unknown sender)' }}</span>
                    @if (e.keywords['$draft']) {
                      <span rmBadge color="warning" class="shrink-0 self-center">Draft</span>
                    }
                    @if (e.keywords['$answered']) {
                      <rm-icon name="reply" class="size-3.5 self-center text-dimmed" role="img" aria-label="Answered" data-testid="answered" />
                    }
                    @if (e.keywords['$forwarded']) {
                      <rm-icon name="forward" class="size-3.5 self-center text-dimmed" role="img" aria-label="Forwarded" />
                    }
                    @if (e.keywords['$flagged']) {
                      <rm-icon name="star" class="size-3.5 self-center text-warning [&_path]:fill-current" role="img" aria-label="Flagged" />
                    }
                    <time class="ml-auto shrink-0 text-xs text-dimmed" [attr.datetime]="e.receivedAt" [title]="fullDate(e.sentAt || e.receivedAt)">{{ shortDate(e.receivedAt) }}</time>
                  </span>
                  @if (open) {
                    <span class="w-full truncate text-xs text-muted">{{ addresses(e.from) }} to {{ names(e.to) || 'nobody' }}@if (e.cc?.length) {, cc {{ names(e.cc) }}}</span>
                  } @else {
                    <span class="w-full truncate text-sm text-dimmed">{{ e.preview }}</span>
                  }
                </button>
                <button rmBtn type="button" variant="ghost" color="neutral" size="sm" aria-label="Message actions" [cdkMenuTriggerFor]="menu"><rm-icon name="ellipsis-vertical" class="size-4" /></button>
                <ng-template #menu>
                  <div class="${MENU}" cdkMenu>
                    @if (e.keywords['$draft']) {
                      <button type="button" class="${MENU_ITEM}" cdkMenuItem (cdkMenuItemTriggered)="editDraft(e)"><rm-icon name="pen-line" class="size-4 text-dimmed" />Edit this draft</button>
                    } @else {
                      <button type="button" class="${MENU_ITEM}" cdkMenuItem (cdkMenuItemTriggered)="respond('reply', e)"><rm-icon name="reply" class="size-4 text-dimmed" />Reply</button>
                      <button type="button" class="${MENU_ITEM}" cdkMenuItem (cdkMenuItemTriggered)="respond('replyAll', e)"><rm-icon name="reply-all" class="size-4 text-dimmed" />Reply all</button>
                      <button type="button" class="${MENU_ITEM}" cdkMenuItem (cdkMenuItemTriggered)="respond('forward', e)"><rm-icon name="forward" class="size-4 text-dimmed" />Forward</button>
                      <button type="button" class="${MENU_ITEM}" cdkMenuItem (cdkMenuItemTriggered)="respond('forwardAttached', e)"><rm-icon name="paperclip" class="size-4 text-dimmed" />Forward as attachment</button>
                    }
                    <div class="my-1 h-px bg-accented" role="separator"></div>
                    @if (e.keywords['$flagged']) {
                      <button type="button" class="${MENU_ITEM}" cdkMenuItem (cdkMenuItemTriggered)="store.setKeyword([e.id], '$flagged', false)"><rm-icon name="star-off" class="size-4 text-dimmed" />Remove flag</button>
                    } @else {
                      <button type="button" class="${MENU_ITEM}" cdkMenuItem (cdkMenuItemTriggered)="store.setKeyword([e.id], '$flagged', true)"><rm-icon name="star" class="size-4 text-dimmed" />Flag</button>
                    }
                    <button type="button" class="${MENU_ITEM}" cdkMenuItem (cdkMenuItemTriggered)="unreadFrom(e)"><rm-icon name="mail" class="size-4 text-dimmed" />Mark unread from here</button>
                    <button type="button" class="${MENU_ITEM}" cdkMenuItem (cdkMenuItemTriggered)="download(e)"><rm-icon name="download" class="size-4 text-dimmed" />Download original</button>
                    <div class="my-1 h-px bg-accented" role="separator"></div>
                    <button type="button" class="${MENU_ITEM} !text-error" cdkMenuItem (cdkMenuItemTriggered)="removeOne(e)">
                      <rm-icon name="trash-2" class="size-4" />{{ inTrash(e) ? 'Delete this message forever' : 'Delete this message' }}
                    </button>
                  </div>
                </ng-template>
              </div>
              @if (open) {
                <div class="flex flex-col gap-3 px-3 pb-3 sm:px-4 sm:pb-4">
                  @let boxes = labels(e);
                  @if (boxes.length > 1 || !e.mailboxIds[view.mailboxId()]) {
                    <div class="flex flex-wrap gap-1.5" aria-label="Mailboxes">
                      @for (m of boxes; track m.id) {
                        <span rmBadge color="neutral">{{ m.name }}</span>
                      }
                    </div>
                  }
                  @if (deliveries(e); as d) {
                    @if (d.canceled) {
                      <p class="text-xs text-dimmed" data-testid="delivery">This message was taken back before it was sent.</p>
                    } @else {
                      <ul class="flex flex-col gap-1" aria-label="Delivery" data-testid="delivery">
                        @for (x of d.list; track x.address) {
                          <li class="flex flex-wrap items-baseline gap-x-2 text-xs" [attr.data-delivery]="x.label">
                            <span rmBadge [color]="x.color" class="self-center"><rm-icon [name]="x.icon" class="size-3" />{{ x.label }}</span>
                            <span class="text-muted">{{ x.address }}</span>
                            @if (x.detail) {
                              <span class="min-w-0 break-words" [class]="x.color === 'error' ? 'text-error' : 'text-dimmed'">{{ x.detail }}</span>
                            }
                          </li>
                        }
                      </ul>
                    }
                  }
                  @if (e.bodyValues !== undefined) {
                    <rm-message-body [email]="e" />
                  } @else {
                    <div class="h-24 w-full ${SKELETON}"></div>
                  }
                  @if (e.keywords['$draft']) {
                    <div>
                      <button rmBtn type="button" variant="subtle" color="neutral" size="sm" data-testid="edit-draft" (click)="editDraft(e)"><rm-icon name="pen-line" class="size-4" />Edit this draft</button>
                    </div>
                  }
                </div>
              }
            </article>
          }
          @if (answerable()) {
            <div class="flex flex-wrap gap-2 px-1 pb-2">
              <button rmBtn type="button" variant="outline" color="neutral" data-testid="reply" (click)="respond('reply')"><rm-icon name="reply" class="size-4" />Reply</button>
              <button rmBtn type="button" variant="outline" color="neutral" data-testid="reply-all" (click)="respond('replyAll')"><rm-icon name="reply-all" class="size-4" />Reply all</button>
              <button rmBtn type="button" variant="outline" color="neutral" data-testid="forward" (click)="respond('forward')"><rm-icon name="forward" class="size-4" />Forward</button>
            </div>
          }
        </div>
      }
    </div>
  `,
})
export class ThreadView {
  protected readonly view = inject(MailView)
  protected readonly store = store
  protected readonly addressList = addressList
  protected readonly fullDate = fullDate
  protected readonly shortDate = shortDate

  protected readonly emails = computed(() => store.threadEmails(this.view.threadId()))
  protected readonly loading = signal(false)
  protected readonly error = signal('')
  // Which messages are open. The rest show one line.
  protected readonly expanded = signal(new Set<Id>())

  // The thread as a row: what the toolbar acts on. Built from the newest
  // email in this mailbox, also when the list does not show the thread.
  protected readonly row = computed(() => {
    const current = this.view.currentRow()
    if (current) return current
    const mailboxId = this.view.mailboxId()
    const inBox = this.emails().filter((e) => e.mailboxIds[mailboxId])
    const newest = (inBox.length ? inBox : this.emails()).at(-1)
    return newest ? buildRow(newest.id, mailboxId, false) : null
  })
  protected readonly subject = computed(() => this.emails()[0]?.subject || '(no subject)')
  protected readonly isTrash = computed(() => this.view.mailbox()?.role === 'trash')
  protected readonly answerable = computed(() => this.emails().some((e) => !e.keywords.$draft))

  // Labels: a mailbox is ticked when every message of the thread is in it.
  protected readonly checked = computed(() => {
    const emails = this.emails()
    return Object.keys(store.state.mailboxes).filter((id) => emails.length > 0 && emails.every((e) => e.mailboxIds[id]))
  })

  constructor() {
    effect(() => {
      const id = this.view.threadId()
      untracked(() => void this.load(id))
    })

    // A message that arrived in the open thread has no body yet.
    const missing = computed(() => this.emails().filter((e) => e.bodyValues === undefined).map((e) => e.id).join(' '))
    effect(() => {
      const ids = missing()
      untracked(() => {
        if (!ids || this.loading()) return
        const list = ids.split(' ')
        void store.loadBodies(list)
        this.expanded.update((s) => new Set([...s, ...list]))
      })
    })

    // The thread emptied under us: deleted here or somewhere else.
    const count = computed(() => this.emails().length)
    let before = 0
    effect(() => {
      const n = count()
      const old = before
      before = n
      if (n === 0 && old > 0) untracked(() => void this.view.closeThread())
    })

    // What became of a message we sent, per recipient. The submissions of
    // the messages in Sent are asked for once and then follow the changes.
    const sent = computed(() => {
      const sentId = store.mailboxByRole('sent')?.id
      return this.emails().filter((e) => sentId && e.mailboxIds[sentId]).map((e) => e.id).join(' ')
    })
    effect(() => {
      const ids = sent()
      if (ids) untracked(() => void compose.loadSubmissions(ids.split(' ')))
    })
  }

  private async load(id: Id): Promise<void> {
    if (!id) return
    this.error.set('')
    this.loading.set(store.threadEmails(id).length === 0)
    try {
      await store.openThread(id)
      if (id !== this.view.threadId()) return
      const list = store.threadEmails(id)
      if (list.length === 0) {
        this.error.set('This conversation no longer exists.')
        return
      }
      // Open what is unread, and always the last message.
      const unread = list.filter((e) => !e.keywords.$seen)
      this.expanded.set(new Set([...unread.map((e) => e.id), list.at(-1)!.id]))
      if (unread.length) void store.setKeyword(unread.map((e) => e.id), '$seen', true)
    } catch (e) {
      if (id === this.view.threadId()) this.error.set(e instanceof Error ? e.message : String(e))
    } finally {
      if (id === this.view.threadId()) this.loading.set(false)
    }
  }

  protected toggle(id: Id): void {
    const next = new Set(this.expanded())
    if (next.has(id)) next.delete(id)
    else next.add(id)
    this.expanded.set(next)
  }

  protected names(list: Email['from']): string {
    return (list ?? []).map(displayName).join(', ')
  }
  protected addresses(list: Email['from']): string {
    return (list ?? []).map((a) => a.email).join(', ')
  }

  // Thread actions.
  markUnread(): void {
    const r = this.row()
    if (!r) return
    void actions.setRead([r], false)
    void this.view.closeThread() // or opening it would mark it read again
  }
  flag(): void {
    const r = this.row()
    if (r) void actions.toggleFlag([r])
  }
  protected move(to: Id): void {
    const r = this.row()
    if (!r) return
    const from = this.view.mailboxId()
    void this.view.leave()
    void actions.move([r], from, to)
  }
  remove(): void {
    const r = this.row()
    if (!r) return
    void this.view.leave()
    void actions.remove([r])
  }

  // Reply, reply all, forward: about one message, or without one the last
  // message of the thread that is not a draft. It answers whether a
  // composer opened: a message whose body is not loaded yet cannot be quoted.
  respond(kind: Exclude<Kind, 'new'>, email?: Email): boolean {
    const e = email ?? this.emails().filter((x) => !x.keywords.$draft).at(-1)
    if (!e || e.bodyValues === undefined || !compose.state.identitiesLoaded) return false
    compose.respond(kind, e)
    return true
  }

  protected async editDraft(e: Email): Promise<void> {
    try {
      await compose.openDraft(e.id)
    } catch (err) {
      store.onError(err instanceof Error ? err.message : String(err))
    }
  }

  protected deliveries(e: Email): { canceled: boolean; list: Delivery[] } | null {
    const s = compose.state.submissions[e.id]
    if (!s) return null
    if (s.undoStatus === 'canceled') return { canceled: true, list: [] }
    return { canceled: false, list: Object.entries(s.deliveryStatus ?? {}).map(([a, d]) => delivery(a, d, s.undoStatus === 'pending')) }
  }

  protected label(id: Id, on: boolean): void {
    const leaves = !on && id === this.view.mailboxId()
    if (leaves) void this.view.leave()
    void store.label(this.emails().map((e) => e.id), id, on)
  }

  protected labels(e: Email) {
    return Object.keys(e.mailboxIds).map((id) => store.state.mailboxes[id]).filter((m) => !!m)
  }

  protected inTrash(e: Email): boolean {
    const trash = store.mailboxByRole('trash')
    return !!trash && !!e.mailboxIds[trash.id]
  }
  protected unreadFrom(e: Email): void {
    const emails = this.emails()
    const from = emails.findIndex((x) => x.id === e.id)
    void store.setKeyword(emails.slice(from).map((x) => x.id), '$seen', false)
    void this.view.closeThread()
  }
  protected download(e: Email): void {
    window.location.assign(client.downloadUrl(e.blobId, `${(e.subject || 'message').slice(0, 80)}.eml`, 'message/rfc822'))
  }
  protected removeOne(e: Email): void {
    if (this.emails().length === 1) void this.view.leave()
    void store.remove([e.id])
  }
}
