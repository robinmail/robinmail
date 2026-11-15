// The three panes: mailboxes, the message list, the open thread. On a
// narrow screen the mailboxes are a drawer and the thread replaces the list.

import { CdkConnectedOverlay, CdkOverlayOrigin } from '@angular/cdk/overlay'
import { ChangeDetectionStrategy, Component, type OnDestroy, type OnInit, computed, effect, inject, signal, untracked, viewChild } from '@angular/core'
import { Router, RouterLink } from '@angular/router'
import { Push, authApi } from '@robinmail/jmap'
import { auth, compose, loadAccount, store } from '../app'
import { FilterBox } from '../components/filter-box'
import { MailboxNav } from '../components/mailbox-nav'
import { MessageList } from '../components/message-list'
import { QuotaMeter } from '../components/quota-meter'
import { ThreadView } from '../components/thread-view'
import { MailView, actions } from '../mail-view'
import { ColorModeButton } from '../ui/color-mode'
import { Badge, Btn, SKELETON } from '../ui/controls'
import { Icon } from '../ui/icon'
import { Toaster } from '../ui/toast'

const POLL_MS = 30_000
// The line between two panes: drag it, or focus it and use the arrow keys.
const HANDLE = 'relative z-10 -mx-1 w-2 shrink-0 cursor-col-resize touch-none outline-none max-lg:hidden after:absolute after:inset-y-0 after:left-1/2 after:w-0.5 after:-translate-x-1/2 hover:after:bg-primary/50 focus-visible:after:bg-primary'
const PANES_KEY = 'robinmail-ng-panes'
type Pane = 'nav' | 'list'
const ICON_BTN = 'inline-flex items-center rounded-md p-1.5 text-default hover:bg-elevated focus-visible:outline-2 focus-visible:outline-primary'
const sendKey = /Mac|iPhone|iPad/.test(navigator.platform) ? '⌘ Enter' : 'Ctrl Enter'

@Component({
  selector: 'rm-mail-page',
  imports: [RouterLink, CdkConnectedOverlay, CdkOverlayOrigin, Btn, Badge, Icon, ColorModeButton, FilterBox, MailboxNav, MessageList, QuotaMeter, ThreadView],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'block h-full' },
  template: `
    @if (loadError()) {
      <div class="flex min-h-full flex-col items-center justify-center gap-4 p-6 text-center">
        <rm-icon name="cloud-off" class="size-10 text-dimmed" />
        <p class="text-muted">The mail could not be loaded.<br><span class="text-sm">{{ loadError() }}</span></p>
        <button rmBtn type="button" (click)="load()">Try again</button>
      </div>
    } @else {
      @let mailbox = view.mailbox();
      @let threadId = view.threadId();
      <div class="flex h-full overflow-hidden" [style.--nav-w.px]="widths().nav" [style.--list-w.px]="widths().list">
        <!-- The mailboxes: a column on a wide screen, a drawer on a narrow one. -->
        @if (drawer()) {
          <div class="fixed inset-0 z-30 bg-black/40 lg:hidden" (click)="drawer.set(false)"></div>
        }
        <aside
          id="mailboxes" class="fixed inset-y-0 left-0 z-40 w-72 max-w-[85vw] flex-col border-r border-default bg-muted lg:static lg:z-auto lg:flex lg:w-(--nav-w) lg:shrink-0"
          [class]="drawer() ? 'flex' : 'hidden'" (click)="drawerClick($event)"
        >
          <div class="flex h-(--ui-header-height) shrink-0 items-center px-4">
            <a routerLink="/mail" class="flex items-center gap-2 rounded focus-visible:outline-2 focus-visible:outline-primary">
              <img src="/favicon.svg" alt="" class="size-6">
              <span class="font-semibold tracking-tight text-highlighted">robinmail</span>
            </a>
          </div>
          <div class="flex min-h-0 flex-1 flex-col overflow-y-auto px-3 pb-3">
            <button rmBtn type="button" [block]="true" class="mb-3" data-testid="compose" [disabled]="!compose.state.identitiesLoaded" (click)="compose.compose()">
              <rm-icon name="pen-line" class="size-4" />New message
            </button>
            @if (store.state.ready) {
              <rm-mailbox-nav />
              <rm-quota-meter class="mt-auto block pt-4" />
            } @else {
              <div class="flex flex-col gap-2 px-1" aria-busy="true">
                @for (n of [1, 2, 3, 4, 5]; track n) {
                  <div class="h-7 w-full ${SKELETON}"></div>
                }
              </div>
            }
          </div>
          <div class="flex w-full min-w-0 shrink-0 items-center gap-0.5 border-t border-default px-3 py-2">
            @let who = store.state.username || auth.account?.username;
            <span class="min-w-0 flex-1 truncate text-sm text-muted" [title]="who">{{ who }}</span>
            <button
              type="button" class="${ICON_BTN} max-lg:hidden" aria-label="Keyboard shortcuts" title="Keyboard shortcuts" cdkOverlayOrigin #keys="cdkOverlayOrigin"
              (click)="shortcutsOpen.set(!shortcutsOpen())"
            >
              <rm-icon name="keyboard" class="size-4" />
            </button>
            <ng-template
              cdkConnectedOverlay [cdkConnectedOverlayOrigin]="keys" [cdkConnectedOverlayOpen]="shortcutsOpen()" [cdkConnectedOverlayHasBackdrop]="true"
              cdkConnectedOverlayBackdropClass="cdk-overlay-transparent-backdrop" [cdkConnectedOverlayPositions]="above"
              (backdropClick)="shortcutsOpen.set(false)" (detach)="shortcutsOpen.set(false)"
            >
              <dl class="mb-1 grid grid-cols-[auto_1fr] items-center gap-x-3 gap-y-1.5 rounded-md bg-default p-4 text-sm shadow-lg ring ring-default">
                @for (s of shortcuts; track s[0]) {
                  <dt><kbd class="rounded bg-elevated px-1.5 py-0.5 font-sans text-xs font-medium ring ring-inset ring-accented">{{ s[0] }}</kbd></dt>
                  <dd class="text-muted">{{ s[1] }}</dd>
                }
              </dl>
            </ng-template>
            <a routerLink="/settings" class="${ICON_BTN}" aria-label="Identities and signatures" title="Identities and signatures"><rm-icon name="settings" class="size-4" /></a>
            <a routerLink="/settings/security" class="${ICON_BTN}" aria-label="Password and security" title="Password and security"><rm-icon name="shield-check" class="size-4" /></a>
            <rm-color-mode-button />
            <button type="button" class="${ICON_BTN}" aria-label="Log out" title="Log out" data-testid="logout" (click)="signOut()"><rm-icon name="log-out" class="size-4" /></button>
          </div>
        </aside>
        <div
          class="${HANDLE}" role="separator" aria-orientation="vertical" aria-label="Width of the mailboxes" tabindex="0" [attr.aria-valuenow]="widths().nav"
          [attr.aria-valuemin]="limits.nav[0]" [attr.aria-valuemax]="limits.nav[1]" (pointerdown)="resize($event, 'nav')" (keydown)="resizeKey($event, 'nav')"
        ></div>

        <section id="list" class="min-w-0 flex-col border-r border-default max-lg:w-full lg:w-(--list-w) lg:shrink-0" [class]="threadId ? 'hidden lg:flex' : 'flex'">
          <div class="flex h-(--ui-header-height) shrink-0 items-center gap-1.5 border-b border-default px-3 sm:px-4">
            <button rmBtn type="button" variant="ghost" color="neutral" class="lg:hidden" aria-label="Mailboxes menu" [attr.aria-expanded]="drawer()" aria-controls="mailboxes" (click)="drawer.set(true)">
              <rm-icon name="menu" class="size-4" />
            </button>
            <h1 class="truncate font-semibold text-highlighted">{{ mailbox?.name ?? 'Mail' }}</h1>
            @if (mailbox && mailbox.unreadEmails > 0) {
              <span rmBadge [attr.aria-label]="mailbox.unreadEmails + ' unread'">{{ mailbox.unreadEmails }}</span>
            }
            <div class="ml-auto flex items-center gap-1">
              @if (store.state.offline) {
                <rm-icon name="cloud-off" class="size-4 text-warning" role="img" aria-label="Offline" title="The server does not answer. Trying again." />
              }
              <button rmBtn type="button" variant="ghost" color="neutral" aria-label="Check for new mail" title="Check for new mail" (click)="refresh(true)">
                <rm-icon name="refresh-cw" class="size-4" />
              </button>
              <rm-filter-box />
              <button rmBtn type="button" aria-label="New message" title="New message (C)" class="lg:hidden" data-testid="compose-small" [disabled]="!compose.state.identitiesLoaded" (click)="compose.compose()">
                <rm-icon name="pen-line" class="size-4" />
              </button>
            </div>
          </div>
          <rm-message-list />
        </section>
        <div
          class="${HANDLE}" role="separator" aria-orientation="vertical" aria-label="Width of the message list" tabindex="0" [attr.aria-valuenow]="widths().list"
          [attr.aria-valuemin]="limits.list[0]" [attr.aria-valuemax]="limits.list[1]" (pointerdown)="resize($event, 'list')" (keydown)="resizeKey($event, 'list')"
        ></div>

        <section id="thread" class="min-w-0 flex-1 flex-col" [class]="threadId ? 'flex' : 'hidden lg:flex'">
          @if (threadId && store.state.ready) {
            <rm-thread-view />
          } @else if (!threadId) {
            <div class="flex flex-1 flex-col items-center justify-center gap-3 text-dimmed">
              <rm-icon name="mail-open" class="size-16" />
              <p class="text-sm">Choose a conversation to read it.</p>
            </div>
          }
        </section>
      </div>
    }
  `,
})
export class MailPage implements OnInit, OnDestroy {
  protected readonly view = inject(MailView)
  private readonly router = inject(Router)
  private readonly toaster = inject(Toaster)
  protected readonly store = store
  protected readonly compose = compose
  protected readonly auth = auth

  protected readonly loadError = signal('')
  protected readonly drawer = signal(false)
  protected readonly shortcutsOpen = signal(false)
  protected readonly above = [{ originX: 'end', originY: 'top', overlayX: 'end', overlayY: 'bottom' } as const]
  // How wide the mailboxes and the list are on a wide screen: the user's
  // choice, kept in this browser.
  protected readonly limits: Record<Pane, [number, number]> = { nav: [192, 400], list: [320, 720] }
  protected readonly widths = signal<Record<Pane, number>>(this.storedWidths())
  private readonly thread = viewChild(ThreadView)
  private readonly messageList = viewChild(MessageList)

  protected readonly shortcuts = [
    ['C', 'New message'], ['R', 'Reply'], ['A', 'Reply all'], ['F', 'Forward'], [sendKey, 'Send'],
    ['J', 'Next conversation'], ['K', 'Previous conversation'], ['U', 'Mark read or unread'], ['S', 'Flag'],
    ['#', 'Delete'], ['/', 'Search'], ['Esc', 'Back to the list'],
  ] as const

  // Changes come in over push (the event source of jmap/push.ts): the server
  // says that something changed and the page fetches it. The timer is the
  // fallback: while push is not connected it asks every half minute. Coming
  // back to the page asks too.
  private readonly push = new Push({
    url: () => store.client.eventSourcePath(),
    onChange: () => store.state.ready && this.refresh(true, true),
  })
  private timer: ReturnType<typeof setInterval> | undefined
  private fast: ReturnType<typeof setInterval> | undefined
  private last = 0
  private pumping = false
  private pumpAgain = false

  private readonly onFocus = () => this.refresh()
  private readonly onKeydown = (e: KeyboardEvent) => this.onKey(e)

  constructor() {
    store.onError = (message) => this.toaster.error(message)

    // The route decides what is shown. Without a mailbox, or with one that
    // does not exist (any more), it is the inbox.
    const ready = computed(() => store.state.ready)
    effect(() => {
      const isReady = ready()
      const s = this.view.spec()
      const onMail = this.view.onMail()
      untracked(() => {
        if (!isReady || !onMail) return
        if (!store.state.mailboxes[s.mailboxId]) {
          const inbox = store.mailboxByRole('inbox') ?? Object.values(store.state.mailboxes)[0]
          if (inbox) void this.router.navigate(['/mail', inbox.id], { replaceUrl: true })
          return
        }
        void store.openList(s)
      })
    })
    const listKey = computed(() => store.state.list.key)
    effect(() => {
      listKey()
      untracked(() => this.view.selection.set(new Set()))
    })
    // The open mailbox was deleted.
    const exists = computed(() => !!store.state.mailboxes[this.view.mailboxId()])
    effect(() => {
      const there = exists()
      untracked(() => {
        if (store.state.ready && !there && this.view.mailboxId() && this.view.onMail()) void this.router.navigate(['/mail'], { replaceUrl: true })
      })
    })
    // On a narrow screen the drawer closes when the route changes.
    effect(() => {
      this.view.mailboxId()
      untracked(() => this.drawer.set(false))
    })

    // Right after a send things move fast: the undo time runs out, the queue
    // delivers, a bounce may come back. Look every three seconds until the
    // submissions are settled.
    const busy = computed(() => compose.unsettled() || compose.state.sent.length > 0)
    effect(() => {
      const b = busy()
      clearInterval(this.fast)
      this.fast = b ? setInterval(() => (compose.unsettled() || compose.state.sent.length > 0 ? this.refresh(true) : clearInterval(this.fast)), 3000) : undefined
    })

    // The title of the tab: unread in the inbox, and where we are.
    const title = computed(() => {
      const unread = store.mailboxByRole('inbox')?.unreadEmails ?? 0
      return `${unread ? `(${unread}) ` : ''}${this.view.mailbox()?.name ?? 'Mail'} · robinmail`
    })
    effect(() => {
      document.title = title()
    })
  }

  ngOnInit(): void {
    if (!store.state.ready || !compose.state.identitiesLoaded) void this.load()
    this.timer = setInterval(() => this.push.connected || this.refresh(true), POLL_MS)
    this.push.start()
    window.addEventListener('focus', this.onFocus)
    window.addEventListener('online', this.onFocus)
    document.addEventListener('visibilitychange', this.onFocus)
    window.addEventListener('keydown', this.onKeydown)
  }

  ngOnDestroy(): void {
    this.push.stop()
    clearInterval(this.timer)
    clearInterval(this.fast)
    window.removeEventListener('focus', this.onFocus)
    window.removeEventListener('online', this.onFocus)
    document.removeEventListener('visibilitychange', this.onFocus)
    window.removeEventListener('keydown', this.onKeydown)
  }

  protected async load(): Promise<void> {
    this.loadError.set('')
    try {
      await loadAccount()
    } catch (e) {
      if (auth.account) this.loadError.set(e instanceof Error ? e.message : String(e))
    }
  }

  protected refresh(force = false, hidden = false): void {
    // A hidden page does not ask on its own, but it does follow push: one
    // request per change, and the title shows the new unread count.
    if (!hidden && document.visibilityState !== 'visible') return
    const now = Date.now()
    if (!force && now - this.last < 2000) return
    this.last = now
    void this.pump()
  }

  // One round of requests at a time, whoever asks: push can report several
  // changes in a second (a draft being saved, a message on its way out), and
  // the server takes four requests at once per account. A round asked for
  // while one runs makes one more follow.
  private async pump(): Promise<void> {
    if (this.pumping) {
      this.pumpAgain = true
      return
    }
    this.pumping = true
    try {
      do {
        this.pumpAgain = false
        await Promise.allSettled([store.refresh(), compose.refreshSubmissions()])
      } while (this.pumpAgain)
    } finally {
      this.pumping = false
    }
  }

  private storedWidths(): Record<Pane, number> {
    const widths = { nav: 256, list: 416 }
    try {
      const stored = JSON.parse(localStorage.getItem(PANES_KEY) ?? '{}') as Partial<Record<Pane, unknown>>
      for (const pane of ['nav', 'list'] as const) {
        const v = stored[pane]
        if (typeof v === 'number' && Number.isFinite(v)) widths[pane] = this.clamp(pane, v)
      }
    } catch {
      // No storage, or not ours: the widths we start with.
    }
    return widths
  }
  private clamp(pane: Pane, width: number): number {
    const [min, max] = this.limits[pane]
    return Math.round(Math.min(max, Math.max(min, width)))
  }
  private setWidth(pane: Pane, width: number, keep: boolean): void {
    this.widths.update((w) => ({ ...w, [pane]: this.clamp(pane, width) }))
    if (!keep) return
    try {
      localStorage.setItem(PANES_KEY, JSON.stringify(this.widths()))
    } catch {
      // It holds until the page is closed.
    }
  }

  protected resize(e: PointerEvent, pane: Pane): void {
    if (e.button !== 0) return
    e.preventDefault()
    const handle = e.currentTarget as HTMLElement
    const from = e.clientX
    const was = this.widths()[pane]
    handle.setPointerCapture(e.pointerId)
    const move = (m: PointerEvent) => this.setWidth(pane, was + m.clientX - from, false)
    const done = () => {
      handle.removeEventListener('pointermove', move)
      handle.removeEventListener('pointerup', done)
      handle.removeEventListener('pointercancel', done)
      this.setWidth(pane, this.widths()[pane], true)
    }
    handle.addEventListener('pointermove', move)
    handle.addEventListener('pointerup', done)
    handle.addEventListener('pointercancel', done)
  }

  protected resizeKey(e: KeyboardEvent, pane: Pane): void {
    const by = e.key === 'ArrowLeft' ? -16 : e.key === 'ArrowRight' ? 16 : 0
    if (!by) return
    e.preventDefault()
    this.setWidth(pane, this.widths()[pane] + by, true)
  }

  // A click on a link in the drawer closes it.
  protected drawerClick(e: MouseEvent): void {
    if ((e.target as HTMLElement).closest('a')) this.drawer.set(false)
  }

  // Keyboard: j/k next and previous, u read/unread, s flag, # or Delete
  // delete, Escape back to the list, / the search box, c a new message, and in
  // an open conversation r reply, a reply all, f forward.
  private onKey(e: KeyboardEvent): void {
    if (e.defaultPrevented || e.metaKey || e.ctrlKey || e.altKey) return
    const t = e.target as HTMLElement | null
    if (t && (t.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(t.tagName))) return
    if (document.querySelector('[role="dialog"], [role="menu"], [role="listbox"]')) return
    const view = this.view
    const selected = view.selectedRows()
    const current = view.currentRow()
    const rows = selected.length ? selected : current ? [current] : []
    const threadId = view.threadId()
    const open = rows.length === 1 && rows[0]!.threadId === threadId && selected.length === 0
    const thread = this.thread()
    switch (e.key) {
      case 'j':
        view.step(1)
        break
      case 'k':
        view.step(-1)
        break
      case 'u':
        if (open) thread?.markUnread()
        else if (rows.length) void actions.toggleRead(rows)
        break
      case 's':
        if (rows.length) void actions.toggleFlag(rows)
        break
      case '#':
      case 'Delete':
        if (open) thread?.remove()
        else if (rows.length) {
          view.selection.set(new Set())
          void actions.remove(rows)
        }
        break
      case 'Escape':
        if (this.drawer()) this.drawer.set(false)
        else if (selected.length) view.selection.set(new Set())
        else if (threadId) void view.closeThread()
        break
      case '/':
        this.messageList()?.focusSearch()
        break
      // "c", a new message, is heard by the frame around the pages.
      case 'r':
      case 'a':
      case 'f':
        if (!threadId || !thread?.respond(e.key === 'r' ? 'reply' : e.key === 'a' ? 'replyAll' : 'forward')) return
        break
      default:
        return
    }
    e.preventDefault()
  }

  protected async signOut(): Promise<void> {
    // What is being written goes to Drafts first. If that fails the user
    // stays, with the composer saying why.
    for (const c of [...compose.state.composers]) {
      if (!(await compose.close(c))) {
        c.minimized = false
        return
      }
    }
    try {
      await authApi.logout()
    } finally {
      auth.account = null
      compose.clear()
      store.reset()
      await this.router.navigate(['/login'], { replaceUrl: true })
    }
  }
}

