// The body of one message. An HTML body is the server's sanitised view in
// an iframe that may run no script and has no origin of ours: the page
// cannot reach into it and it cannot reach the session. Message HTML never
// becomes part of this page's own DOM. A text body is shown as text.

import { ChangeDetectionStrategy, Component, type ElementRef, computed, effect, input, signal, untracked, viewChild } from '@angular/core'
import type { Email } from '@robinmail/jmap'
import { client } from '../app'
import { Btn } from '../ui/controls'
import { Icon } from '../ui/icon'
import { fileSize } from '../util/format'

@Component({
  selector: 'rm-message-body',
  imports: [Btn, Icon],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'flex flex-col gap-3' },
  template: `
    @if (html()) {
      @if (remote() === 'blocked' && !images()) {
        <div class="flex flex-wrap items-center gap-2 rounded-md bg-elevated px-3 py-2 text-sm text-muted">
          <rm-icon name="image-off" class="size-4" />
          <span class="mr-auto">Remote images are not loaded: they tell the sender you opened this.</span>
          <button rmBtn type="button" variant="outline" color="neutral" size="xs" data-testid="load-images" (click)="images.set(true)">Load remote images</button>
        </div>
      }
      <!-- Mail is written for a white page, so the frame is white in dark mode too. -->
      <div #frame class="contents"></div>
    } @else {
      <pre class="whitespace-pre-wrap break-words font-sans text-sm leading-relaxed text-default" data-testid="text-view">@if (!text()) {(This message has no text.)}@for (p of linked(); track $index) {@if (p.href) {<a
        [href]="p.href" [attr.target]="p.href.startsWith('mailto:') ? null : '_blank'" rel="noopener noreferrer" class="text-primary underline underline-offset-2"
      >{{ p.text }}</a>} @else {{{ p.text }}}}</pre>
      @if (truncated()) {
        <p class="text-xs text-dimmed">This message is too long to show in full.</p>
      }
    }

    @if (attachments().length > 0) {
      <ul class="flex flex-wrap gap-2" aria-label="Attachments">
        @for (a of attachments(); track a.blobId) {
          <li>
            <a
              [href]="client.downloadUrl(a.blobId!, a.name || 'attachment', a.type)" [attr.download]="a.name || 'attachment'"
              class="flex max-w-64 items-center gap-2 rounded-md border border-default px-3 py-2 text-sm hover:bg-elevated focus-visible:outline-2 focus-visible:outline-primary"
              data-testid="attachment"
            >
              <rm-icon name="paperclip" class="size-4 text-dimmed" />
              <span class="truncate">{{ a.name || 'attachment' }}</span>
              <span class="shrink-0 text-xs text-dimmed">{{ fileSize(a.size) }}</span>
              <rm-icon name="download" class="size-4 text-dimmed" />
            </a>
          </li>
        }
      </ul>
    }
  `,
})
export class MessageBody {
  readonly email = input.required<Email>()
  protected readonly client = client
  protected readonly fileSize = fileSize

  protected readonly html = computed(() => this.email().htmlBody?.some((p) => p.type === 'text/html') ?? false)
  protected readonly text = computed(() => {
    const values = this.email().bodyValues ?? {}
    return (this.email().textBody ?? [])
      .map((p) => (p.partId !== null ? values[p.partId] : undefined))
      .filter((v) => v !== undefined)
      .map((v) => v.value)
      .join('\n')
  })
  // The text with its links: web addresses open in a new tab, mail
  // addresses in the composer (the page catches mailto: links). The pieces
  // are text nodes and anchors made by the template, never markup.
  protected readonly linked = computed(() => {
    const out: { text: string; href?: string }[] = []
    const re = /\bhttps?:\/\/[^\s<>"')\]]+[^\s<>"')\].,;:!?]|[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+/g
    const t = this.text()
    let at = 0
    for (let m = re.exec(t); m; m = re.exec(t)) {
      if (m.index > at) out.push({ text: t.slice(at, m.index) })
      out.push({ text: m[0], href: m[0].includes('://') ? m[0] : `mailto:${m[0]}` })
      at = m.index + m[0].length
    }
    if (at < t.length) out.push({ text: t.slice(at) })
    return out
  })
  protected readonly truncated = computed(() => Object.values(this.email().bodyValues ?? {}).some((v) => v.isTruncated))

  // Remote images: left out until the reader asks, per message.
  protected readonly images = signal(false)
  protected readonly remote = signal<'blocked' | 'shown' | 'none'>('none')
  private readonly id = computed(() => this.email().id)
  private readonly frame = viewChild<ElementRef<HTMLElement>>('frame')

  constructor() {
    // The frame is made here and not by the template: it has to carry its
    // sandbox and its address before it enters the page. A frame that is
    // in the page first and gets its address after starts as an empty
    // document, which the browser complains about for a sandbox. The
    // address is the server's view of this message, a path of our own
    // origin built by the client. Another message or the choice to load
    // images makes a new frame.
    effect((onCleanup) => {
      const host = this.frame()?.nativeElement
      if (!host) return
      const f = document.createElement('iframe')
      f.setAttribute('sandbox', 'allow-popups allow-popups-to-escape-sandbox')
      f.setAttribute('referrerpolicy', 'no-referrer')
      f.setAttribute('loading', 'lazy')
      f.setAttribute('data-testid', 'html-view')
      f.title = `Message: ${untracked(() => this.email().subject) || '(no subject)'}`
      f.className = 'h-[65vh] min-h-80 w-full rounded-md border border-default bg-white'
      f.src = client.viewUrl(this.id(), this.images())
      host.replaceChildren(f)
      onCleanup(() => f.remove())
    })
    effect(() => {
      const id = this.id()
      const isHtml = this.html()
      untracked(async () => {
        this.images.set(false)
        this.remote.set('none')
        if (!isHtml) return
        try {
          const r = await client.remoteImages(id)
          if (this.id() === id) this.remote.set(r)
        } catch {
          // The view itself shows what is wrong.
        }
      })
    })
  }

  protected readonly attachments = computed(() => (this.email().attachments ?? []).filter((a) => a.blobId))
}
