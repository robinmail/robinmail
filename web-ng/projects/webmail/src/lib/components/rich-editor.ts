// A small rich text editor on contenteditable: bold, italic, lists,
// quotes, links and inline images. The browser does the editing
// (execCommand); what leaves the editor is written by exportHtml, which
// knows only those few elements. Pasted and dropped content comes in as
// plain text, so no foreign markup gets into the page.

import { CdkConnectedOverlay, CdkOverlayOrigin } from '@angular/cdk/overlay'
import { type AfterViewInit, ChangeDetectionStrategy, Component, type ElementRef, input, output, signal, viewChild } from '@angular/core'
import { Btn, Input, LABEL } from '../ui/controls'
import { Icon } from '../ui/icon'
import { exportHtml, imageElement, importHtml, safeHref } from '../util/richdom'

type Mark = 'bold' | 'italic' | 'ul' | 'ol' | 'quote'

@Component({
  selector: 'rm-rich-editor',
  imports: [CdkConnectedOverlay, CdkOverlayOrigin, Btn, Input, Icon],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'flex min-h-0 flex-1 flex-col' },
  template: `
    <div class="flex items-center gap-0.5 border-b border-default px-2 py-1" role="toolbar" aria-label="Formatting">
      @for (b of buttons; track b.key) {
        <button
          rmBtn type="button" size="xs" color="neutral" variant="ghost" [class]="active()[b.key] ? '!bg-accented' : ''" [attr.aria-label]="b.label" [title]="b.label"
          [attr.aria-pressed]="active()[b.key]" (mousedown)="$event.preventDefault()" (click)="b.run()"
        >
          <rm-icon [name]="b.icon" class="size-4" />
        </button>
      }
      <button
        rmBtn type="button" size="xs" color="neutral" variant="ghost" [class]="active().link ? '!bg-accented' : ''" aria-label="Link" title="Link" [attr.aria-pressed]="active().link"
        cdkOverlayOrigin #origin="cdkOverlayOrigin" (mousedown)="$event.preventDefault()" (click)="$event.preventDefault(); openLink()"
      >
        <rm-icon name="link" class="size-4" />
      </button>
      <ng-template
        cdkConnectedOverlay [cdkConnectedOverlayOrigin]="origin" [cdkConnectedOverlayOpen]="linkOpen()" [cdkConnectedOverlayHasBackdrop]="true"
        cdkConnectedOverlayBackdropClass="cdk-overlay-transparent-backdrop" (backdropClick)="linkOpen.set(false)" (detach)="linkOpen.set(false)"
      >
        <form class="mt-1 flex w-72 flex-col gap-2 rounded-md bg-default p-3 shadow-lg ring ring-default" (submit)="$event.preventDefault(); applyLink()">
          <label class="${LABEL}">
            <span>Link address</span>
            <input rmInput #url placeholder="https://example.org" autofocus autocomplete="off" [value]="linkUrl()" (input)="linkUrl.set(url.value)">
          </label>
          @if (linkError()) {
            <p class="text-xs text-error">{{ linkError() }}</p>
          }
          <div class="flex justify-end gap-2">
            @if (active().link) {
              <button rmBtn type="button" size="xs" color="neutral" variant="ghost" (click)="linkUrl.set(''); applyLink()">Remove link</button>
            }
            <button rmBtn type="submit" size="xs">Apply</button>
          </div>
        </form>
      </ng-template>
    </div>
    <div
      #el contenteditable="true" role="textbox" aria-multiline="true" [attr.aria-label]="label()" spellcheck="true" data-testid="rich-body"
      class="rich min-h-0 flex-1 overflow-y-auto overscroll-contain px-3 py-2 text-sm leading-relaxed text-default outline-none"
      (input)="changed()" (paste)="onPaste($event)" (drop)="onDrop($event)" (keyup)="track()" (mouseup)="track()" (blur)="keep()"
    ></div>
  `,
})
export class RichEditor implements AfterViewInit {
  // The HTML the editor starts with. Later changes come from the editor itself.
  readonly html = input.required<string>()
  readonly imageUrl = input.required<(cid: string) => string | null>()
  readonly label = input.required<string>()
  readonly htmlChange = output<string>()
  readonly files = output<File[]>()

  private readonly el = viewChild.required<ElementRef<HTMLDivElement>>('el')
  protected readonly active = signal({ bold: false, italic: false, ul: false, ol: false, quote: false, link: false })
  protected readonly linkOpen = signal(false)
  protected readonly linkUrl = signal('')
  protected readonly linkError = signal('')
  private saved: Range | null = null

  protected readonly buttons: { key: Mark; icon: string; label: string; run: () => void }[] = [
    { key: 'bold', icon: 'bold', label: 'Bold', run: () => this.run('bold') },
    { key: 'italic', icon: 'italic', label: 'Italic', run: () => this.run('italic') },
    { key: 'ul', icon: 'list', label: 'Bulleted list', run: () => this.run('insertUnorderedList') },
    { key: 'ol', icon: 'list-ordered', label: 'Numbered list', run: () => this.run('insertOrderedList') },
    { key: 'quote', icon: 'text-quote', label: 'Quote', run: () => this.toggleQuote() },
  ]

  private get root(): HTMLDivElement {
    return this.el().nativeElement
  }

  ngAfterViewInit(): void {
    this.root.replaceChildren(importHtml(this.html(), this.imageUrl()))
    try {
      document.execCommand('defaultParagraphSeparator', false, 'p')
    } catch {
      // An old browser: it makes its own kind of lines, which export as they are.
    }
  }

  protected changed(): void {
    this.htmlChange.emit(exportHtml(this.root))
    this.track()
  }

  private inside(tag: string): Element | null {
    const sel = document.getSelection()
    let n: Node | null = sel?.anchorNode ?? null
    while (n && n !== this.root) {
      if (n.nodeType === Node.ELEMENT_NODE && (n as Element).tagName === tag) return n as Element
      n = n.parentNode
    }
    return null
  }

  private hasSelection(): boolean {
    const sel = document.getSelection()
    return !!sel && sel.rangeCount > 0 && this.root.contains(sel.anchorNode)
  }

  // track notes what the caret is in, for the pressed state of the buttons.
  protected track(): void {
    if (!this.hasSelection()) return
    this.active.set({
      bold: document.queryCommandState('bold'),
      italic: document.queryCommandState('italic'),
      ul: document.queryCommandState('insertUnorderedList'),
      ol: document.queryCommandState('insertOrderedList'),
      quote: !!this.inside('BLOCKQUOTE'),
      link: !!this.inside('A'),
    })
  }

  private run(command: string, value?: string): void {
    this.root.focus()
    document.execCommand(command, false, value)
    this.changed()
  }

  private toggleQuote(): void {
    this.root.focus()
    if (this.inside('BLOCKQUOTE')) document.execCommand('outdent')
    else document.execCommand('formatBlock', false, 'blockquote')
    this.changed()
  }

  keep(): void {
    const sel = document.getSelection()
    this.saved = sel && this.hasSelection() ? sel.getRangeAt(0).cloneRange() : null
  }

  private restore(): void {
    this.root.focus()
    if (!this.saved) return
    const sel = document.getSelection()
    sel?.removeAllRanges()
    sel?.addRange(this.saved)
  }

  protected openLink(): void {
    this.keep()
    this.linkUrl.set(this.inside('A')?.getAttribute('href') ?? '')
    this.linkError.set('')
    this.linkOpen.set(true)
  }

  protected applyLink(): void {
    let url = this.linkUrl().trim()
    if (url !== '' && !/^[a-z][a-z0-9+.-]*:/i.test(url)) url = url.includes('@') && !url.includes('/') ? `mailto:${url}` : `https://${url}`
    if (url !== '' && !safeHref(url)) {
      this.linkError.set('A link starts with https://, http:// or mailto:')
      return
    }
    this.linkOpen.set(false)
    this.restore()
    if (url === '') document.execCommand('unlink')
    else if (document.getSelection()?.isCollapsed && !this.inside('A')) {
      const a = document.createElement('a')
      a.href = url
      a.textContent = url.replace(/^mailto:/i, '')
      document.getSelection()!.getRangeAt(0).insertNode(a)
      document.getSelection()!.collapse(a.parentNode, [...a.parentNode!.childNodes].indexOf(a) + 1)
    } else document.execCommand('createLink', false, url)
    this.changed()
  }

  protected onPaste(e: ClipboardEvent): void {
    const files = [...(e.clipboardData?.files ?? [])]
    e.preventDefault()
    if (files.length > 0) {
      this.keep()
      this.files.emit(files)
      return
    }
    const text = e.clipboardData?.getData('text/plain') ?? ''
    if (text) document.execCommand('insertText', false, text)
    this.changed()
  }

  protected onDrop(e: DragEvent): void {
    // Files are for the composer around us. Anything else comes in as text.
    if (e.dataTransfer?.files.length) return
    e.preventDefault()
    const text = e.dataTransfer?.getData('text/plain') ?? ''
    if (text) this.run('insertText', text)
  }

  // insertImage puts an uploaded image where the caret was.
  insertImage(cid: string, url: string): void {
    const img = imageElement(cid, url)
    this.restore()
    const sel = document.getSelection()
    if (sel && this.hasSelection()) {
      const r = sel.getRangeAt(0)
      r.deleteContents()
      r.insertNode(img)
      r.setStartAfter(img)
      r.collapse(true)
    } else this.root.appendChild(img)
    this.changed()
  }

  removeImage(cid: string): void {
    for (const img of this.root.querySelectorAll('img')) if (img.dataset['cid'] === cid) img.remove()
    this.changed()
  }

  focus(): void {
    this.root.focus()
  }
}
