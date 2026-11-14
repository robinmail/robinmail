import { ChangeDetectionStrategy, Component, type ElementRef, type OnDestroy, computed, effect, inject, untracked, viewChild } from '@angular/core'
import { Router } from '@angular/router'
import type { ListSpec } from '@robinmail/jmap'
import { compose, store } from '../app'
import { MailView, type Row, actions } from '../mail-view'
import { Btn, SKELETON } from '../ui/controls'
import { Icon } from '../ui/icon'
import { fullDate, shortDate } from '../util/format'
import { type SnippetPart, snippetParts } from '../util/snippet'
import { MoveMenu } from './move-menu'
import { SearchBox } from './search-box'

@Component({
  selector: 'rm-message-list',
  imports: [Btn, Icon, MoveMenu, SearchBox],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'flex min-h-0 flex-1 flex-col' },
  template: `
    <rm-search-box />
    @let rows = view.rows();
    @let picked = view.selectedRows();
    @let list = store.state.list;
    <!-- Selection bar, or the active filters -->
    @if (rows.length > 0 || view.filterCount() > 0) {
      <div class="flex min-h-11 items-center gap-1 border-b border-default px-3 py-1.5">
        @if (rows.length > 0) {
          <input
            #all type="checkbox" aria-label="Select all" class="mr-2" [checked]="allSelected()" [indeterminate]="!allSelected() && picked.length > 0"
            (change)="toggleAll(all.checked)"
          >
        }
        @if (picked.length > 0) {
          <span class="mr-auto text-sm text-muted" data-testid="selected-count">{{ picked.length }} selected</span>
          @let anyUnread = someUnread();
          <button
            rmBtn type="button" variant="ghost" color="neutral" size="sm" [attr.aria-label]="anyUnread ? 'Mark read' : 'Mark unread'"
            [title]="anyUnread ? 'Mark read' : 'Mark unread'" (click)="bulk('read')"
          >
            <rm-icon [name]="anyUnread ? 'mail-open' : 'mail'" class="size-4" />
          </button>
          <button rmBtn type="button" variant="ghost" color="neutral" size="sm" aria-label="Flag" title="Flag" (click)="bulk('flag')"><rm-icon name="star" class="size-4" /></button>
          <rm-move-menu mode="move" size="sm" label="Move to" [exclude]="view.mailboxId()" (move)="bulk('move', $event)" />
          <button
            rmBtn type="button" variant="ghost" color="neutral" size="sm" [attr.aria-label]="isTrash() ? 'Delete forever' : 'Delete'"
            [title]="isTrash() ? 'Delete forever' : 'Delete'" (click)="bulk('remove')"
          >
            <rm-icon name="trash-2" class="size-4" />
          </button>
        } @else {
          <div class="flex flex-wrap items-center gap-1.5">
            @for (c of chips(); track c.label) {
              <span class="inline-flex max-w-48 items-center gap-1 rounded-md bg-elevated px-1.5 py-0.5 text-xs font-medium text-default ring ring-inset ring-accented">
                <span class="truncate">{{ c.label }}</span>
                <button type="button" class="rounded hover:text-highlighted" [attr.aria-label]="'Remove filter ' + c.label" (click)="view.setFilter(c.clear)">
                  <rm-icon name="x" class="size-3 align-middle" />
                </button>
              </span>
            }
            @if (chips().length === 0) {
              <span class="text-xs text-dimmed">{{ list.total }} {{ list.total === 1 ? 'conversation' : 'conversations' }}</span>
            }
          </div>
        }
      </div>
    }

    <div class="min-h-0 flex-1 overflow-y-auto overscroll-contain" data-testid="message-list">
      @if (list.loading && rows.length === 0) {
        <div class="flex flex-col gap-4 p-4" aria-busy="true">
          @for (n of [1, 2, 3, 4, 5, 6]; track n) {
            <div class="flex flex-col gap-2">
              <div class="h-4 w-1/3 ${SKELETON}"></div>
              <div class="h-4 w-2/3 ${SKELETON}"></div>
              <div class="h-3 w-full ${SKELETON}"></div>
            </div>
          }
        </div>
      } @else if (list.error) {
        <div class="flex flex-col items-center gap-3 p-8 text-center">
          <rm-icon name="triangle-alert" class="size-8 text-error" />
          <p class="text-sm text-muted">The list could not be loaded.<br>{{ list.error }}</p>
          <button rmBtn type="button" variant="subtle" color="neutral" (click)="store.openList(view.spec())">Try again</button>
        </div>
      } @else if (rows.length === 0) {
        <div class="flex flex-col items-center gap-3 p-8 pt-16 text-center">
          <rm-icon [name]="view.filterCount() ? 'search-x' : 'inbox'" class="size-10 text-dimmed" />
          <p class="text-sm text-muted">
            {{ view.spec().text ? 'No messages match the search.' : view.filterCount() ? 'No messages match the filter.' : 'No messages here.' }}
          </p>
          @if (view.filterCount()) {
            <button rmBtn type="button" variant="subtle" color="neutral" size="sm" (click)="clearAll()">{{ view.spec().text ? 'Clear the search' : 'Clear the filter' }}</button>
          }
        </div>
      } @else {
        <ul role="list">
          @for (row of rows; track row.id) {
            @let open = row.threadId === view.threadId();
            @let selected = view.selection().has(row.id);
            <li
              [attr.data-row]="row.id" [attr.data-thread]="row.threadId" [attr.data-unread]="row.unread ? 'true' : null"
              class="group relative flex border-b border-l-2 border-default"
              [class]="(open ? 'border-l-primary bg-primary/10' : 'border-l-transparent hover:bg-elevated/50') + (selected && !open ? ' bg-elevated' : '')"
            >
              <div class="flex flex-col items-center gap-2 py-3 pl-2.5 pr-1">
                <input #box type="checkbox" [checked]="selected" [attr.aria-label]="'Select ' + (row.email.subject || 'message')" (change)="toggle(row, box.checked)">
                <button
                  type="button" class="rounded p-0.5 focus-visible:outline-2 focus-visible:outline-primary"
                  [class]="row.flagged ? 'text-warning' : 'text-dimmed hover:text-highlighted focus-visible:opacity-100 lg:opacity-0 lg:group-hover:opacity-100'"
                  [attr.aria-label]="row.flagged ? 'Remove flag' : 'Flag'" [attr.aria-pressed]="row.flagged" data-testid="row-flag" (click)="flag(row)"
                >
                  <rm-icon name="star" class="block size-4" [class]="row.flagged ? '[&_path]:fill-current' : ''" />
                </button>
              </div>
              <a
                [href]="href(row)" class="min-w-0 flex-1 py-2.5 pl-1.5 pr-3 focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-primary"
                [attr.aria-current]="open ? 'true' : null" (click)="openRow($event, row)"
              >
                <div class="flex items-baseline gap-2">
                  <span class="truncate text-sm" [class]="row.unread ? 'font-semibold text-highlighted' : 'text-toned'">{{ row.names }}</span>
                  @if (row.emails.length > 1) {
                    <span class="shrink-0 text-xs tabular-nums text-dimmed" [attr.aria-label]="row.emails.length + ' messages'">{{ row.emails.length }}</span>
                  }
                  <time
                    class="ml-auto shrink-0 text-xs tabular-nums" [class]="row.unread ? 'font-medium text-primary' : 'text-dimmed'"
                    [attr.datetime]="row.email.receivedAt" [title]="fullDate(row.email.receivedAt)"
                  >{{ shortDate(row.email.receivedAt) }}</time>
                </div>
                <div class="flex items-center gap-1.5">
                  @if (row.unread) {
                    <span class="size-2 shrink-0 rounded-full bg-primary" role="img" aria-label="Unread"></span>
                  }
                  <span class="truncate text-sm" [class]="row.unread ? 'font-semibold text-highlighted' : 'text-default'">
                    @if (snippet(row.id, 'subject'); as parts) {
                      @for (part of parts; track $index) {
                        @if (part.mark) {<mark class="rounded-sm bg-warning/30 text-inherit">{{ part.text }}</mark>} @else {{{ part.text }}}
                      }
                    } @else {
                      {{ row.email.subject || '(no subject)' }}
                    }
                  </span>
                  @if (row.email.keywords['$answered']) {
                    <rm-icon name="reply" class="size-3.5 text-dimmed" role="img" aria-label="Answered" />
                  }
                  @if (row.attachment) {
                    <rm-icon name="paperclip" class="ml-auto size-3.5 text-dimmed" role="img" aria-label="Has attachment" />
                  }
                </div>
                @if (snippet(row.id, 'preview'); as parts) {
                  <p class="truncate text-sm text-dimmed" data-testid="snippet">
                    @for (part of parts; track $index) {
                      @if (part.mark) {<mark class="rounded-sm bg-warning/30 text-default">{{ part.text }}</mark>} @else {{{ part.text }}}
                    }
                  </p>
                } @else {
                  <p class="truncate text-sm text-dimmed">{{ row.email.preview || ' ' }}</p>
                }
              </a>
            </li>
          }
        </ul>
      }

      @if (rows.length > 0) {
        <div #sentinel class="flex justify-center p-4">
          @if (list.ids.length < list.total) {
            <button rmBtn type="button" variant="ghost" color="neutral" size="sm" [disabled]="list.loadingMore" (click)="store.loadMore()">Load more</button>
          } @else if (list.total > 10) {
            <span class="text-xs text-dimmed">End of the list</span>
          }
        </div>
      }
    </div>
  `,
})
export class MessageList implements OnDestroy {
  protected readonly view = inject(MailView)
  private readonly router = inject(Router)
  protected readonly store = store
  protected readonly fullDate = fullDate
  protected readonly shortDate = shortDate

  protected readonly isTrash = computed(() => this.view.mailbox()?.role === 'trash')
  protected readonly allSelected = computed(() => this.view.rows().length > 0 && this.view.selectedRows().length === this.view.rows().length)
  protected readonly someUnread = computed(() => this.view.selectedRows().some((r) => r.unread))

  private readonly search = viewChild.required(SearchBox)
  private readonly sentinel = viewChild<ElementRef<HTMLElement>>('sentinel')
  // Infinite scroll: the next page loads when the end of the list shows.
  private readonly observer =
    typeof IntersectionObserver === 'function'
      ? new IntersectionObserver(
          (entries) => {
            if (entries.some((e) => e.isIntersecting)) void store.loadMore()
          },
          { rootMargin: '200px' },
        )
      : undefined

  constructor() {
    effect((onCleanup) => {
      const el = this.sentinel()?.nativeElement
      if (!el || !this.observer) return
      this.observer.observe(el)
      onCleanup(() => this.observer?.unobserve(el))
    })
    // Rows that left the list are not selected any more.
    const rowIds = computed(() => this.view.rows().map((r) => r.id).join(' '))
    effect(() => {
      const have = new Set(rowIds().split(' '))
      untracked(() => {
        const selection = this.view.selection()
        if (selection.size === 0) return
        const kept = [...selection].filter((id) => have.has(id))
        if (kept.length !== selection.size) this.view.selection.set(new Set(kept))
      })
    })
  }

  ngOnDestroy(): void {
    this.observer?.disconnect()
  }

  focusSearch(): void {
    this.search().focus()
  }

  protected toggle(row: Row, on: boolean): void {
    const next = new Set(this.view.selection())
    if (on) next.add(row.id)
    else next.delete(row.id)
    this.view.selection.set(next)
  }
  protected toggleAll(on: boolean): void {
    this.view.selection.set(new Set(on ? this.view.rows().map((r) => r.id) : []))
  }
  protected flag(row: Row): void {
    void actions.toggleFlag([row])
  }

  // An action on the selection. When the open thread is part of it and
  // leaves the list, the view returns to the list.
  protected async bulk(what: 'read' | 'flag' | 'move' | 'remove', to = ''): Promise<void> {
    const picked = this.view.selectedRows()
    if (picked.length === 0) return
    const leaves = what === 'move' || what === 'remove'
    if (leaves && picked.some((r) => r.threadId === this.view.threadId())) void this.view.closeThread()
    const from = this.view.mailboxId()
    this.view.selection.set(new Set())
    if (what === 'read') await actions.toggleRead(picked)
    else if (what === 'flag') await actions.toggleFlag(picked)
    else if (what === 'move') await actions.move(picked, from, to)
    else await actions.remove(picked)
  }

  protected href(row: Row): string {
    return this.router.serializeUrl(this.router.createUrlTree(this.view.threadLink(row.threadId), { queryParams: this.view.filterQuery() }))
  }

  // A plain click opens the thread here; with a modifier key the browser
  // does what it does with a link. In Drafts a row that is one draft opens
  // in the composer, not as a thread.
  protected openRow(e: MouseEvent, row: Row): void {
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return
    e.preventDefault()
    if (this.view.mailbox()?.role === 'drafts' && row.emails.length === 1 && row.email.keywords.$draft) {
      compose.openDraft(row.id).catch((err: unknown) => store.onError(err instanceof Error ? err.message : String(err)))
      return
    }
    void this.view.openThread(row.threadId)
  }

  // What a search found in a row: the subject and a piece of the body with
  // the matches marked. null where nothing matched, or without a search.
  protected snippet(id: string, field: 'subject' | 'preview'): SnippetPart[] | null {
    const html = store.state.list.snippets[id]?.[field]
    return html ? snippetParts(html) : null
  }

  protected clearAll(): void {
    void this.view.setFilter({ text: '', from: '', subject: '', unread: false, flagged: false, attachment: false })
  }

  protected readonly chips = computed(() => {
    const s = this.view.spec()
    const out: { label: string; clear: Partial<ListSpec> }[] = []
    if (s.text) out.push({ label: `Search: ${s.text}`, clear: { text: '' } })
    if (s.from) out.push({ label: `From: ${s.from}`, clear: { from: '' } })
    if (s.subject) out.push({ label: `Subject: ${s.subject}`, clear: { subject: '' } })
    if (s.unread) out.push({ label: 'Unread', clear: { unread: false } })
    if (s.flagged) out.push({ label: 'Flagged', clear: { flagged: false } })
    if (s.attachment) out.push({ label: 'Has attachment', clear: { attachment: false } })
    return out
  })
}
