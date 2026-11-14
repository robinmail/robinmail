// One message being written: the fields, the body as plain or rich text,
// attachments, and the buttons to send, close or discard it.

import { ChangeDetectionStrategy, Component, type ElementRef, afterNextRender, computed, effect, inject, input, signal, untracked, viewChild } from '@angular/core'
import { type Attachment, type Composer, formatAddress } from '@robinmail/jmap'
import { client, compose } from '../app'
import { ALERT, Btn } from '../ui/controls'
import { Icon } from '../ui/icon'
import { Toaster } from '../ui/toast'
import { fileSize } from '../util/format'
import { AddressInput } from './address-input'
import { RichEditor } from './rich-editor'

const names: Record<Composer['kind'], string> = { new: 'New message', reply: 'Reply', replyAll: 'Reply', forward: 'Forward', forwardAttached: 'Forward' }
const sendKeys = /Mac|iPhone|iPad/.test(navigator.platform) ? '⌘ Enter' : 'Ctrl Enter'

@Component({
  selector: 'rm-composer-panel',
  imports: [Btn, Icon, AddressInput, RichEditor],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'contents' },
  template: `
    @let c = composer();
    @let draft = c.draft;
    <section
      [attr.aria-label]="'Message: ' + title()" [attr.data-composer]="c.key" data-testid="composer"
      class="pointer-events-auto relative flex flex-col border border-default bg-default shadow-2xl"
      [class]="c.minimized ? 'w-full rounded-t-lg sm:w-64' : 'max-sm:fixed max-sm:inset-0 max-sm:border-0 sm:h-[min(40rem,calc(100dvh-4.5rem))] sm:w-[36rem] sm:rounded-t-lg'"
      (keydown)="onKey($event)" (dragenter)="hasFiles($event) && dragging.set(dragging() + 1)" (dragleave)="dragging.set(max0(dragging() - 1))"
      (dragover)="hasFiles($event) && $event.preventDefault()" (drop)="onDrop($event)"
    >
      <header class="flex h-10 shrink-0 items-center gap-1 rounded-t-lg bg-elevated pl-3 pr-1" [class]="c.minimized ? '' : 'max-sm:rounded-none'">
        <button
          type="button" class="flex min-w-0 flex-1 items-center gap-2 self-stretch text-left focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-primary"
          [attr.aria-expanded]="!c.minimized" [attr.aria-label]="(c.minimized ? 'Open ' : 'Minimise ') + title()" (click)="c.minimized = !c.minimized"
        >
          <span class="truncate text-sm font-medium text-highlighted">{{ title() }}</span>
          @if (status()) {
            <span class="shrink-0 text-xs" [class]="c.saveError ? 'text-error' : 'text-dimmed'" data-testid="composer-status" aria-live="polite">{{ status() }}</span>
          }
        </button>
        <button rmBtn type="button" size="xs" color="neutral" variant="ghost" [attr.aria-label]="c.minimized ? 'Open' : 'Minimise'" [title]="c.minimized ? 'Open' : 'Minimise'" (click)="c.minimized = !c.minimized">
          <rm-icon [name]="c.minimized ? 'chevron-up' : 'minus'" class="size-4" />
        </button>
        <button rmBtn type="button" size="xs" color="neutral" variant="ghost" aria-label="Save and close" title="Save and close" data-testid="composer-close" (click)="close()">
          <rm-icon name="x" class="size-4" />
        </button>
      </header>

      <div class="min-h-0 flex-1 flex-col" [class]="(c.minimized ? 'hidden' : 'flex') + (c.sending ? ' opacity-60' : '')" [attr.inert]="c.sending ? '' : null">
        @if (c.sendError || c.saveError) {
          <div class="m-2 mb-0 ${ALERT}" role="alert" data-testid="composer-error"><rm-icon name="circle-alert" class="mt-0.5 size-4" /><span>{{ c.sendError || c.saveError }}</span></div>
        }

        <div class="flex flex-col divide-y divide-default border-b border-default px-3 text-sm">
          @if (identities().length > 1) {
            <div class="flex items-center gap-2 py-1">
              <label [for]="uid + '-from'" class="w-12 shrink-0 text-dimmed">From</label>
              <select #from [id]="uid + '-from'" class="min-w-0 flex-1 bg-transparent py-1 text-sm text-highlighted outline-none" (change)="compose.setIdentity(c, from.value)">
                @for (i of identities(); track i.value) {
                  <option [value]="i.value" [selected]="i.value === draft.identityId">{{ i.label }}</option>
                }
              </select>
            </div>
          }
          <div class="flex items-start gap-2">
            <label [for]="uid + '-to'" class="w-12 shrink-0 py-1.5 text-dimmed">To</label>
            <rm-address-input #toField [value]="draft.to" label="To" [inputId]="uid + '-to'" (valueChange)="draft.to = $event; touch()" />
            @if (!showCc()) {
              <button rmBtn type="button" size="xs" color="neutral" variant="ghost" class="mt-1" aria-label="Add Cc and Bcc" (click)="openCc()">Cc Bcc</button>
            }
          </div>
          @if (showCc()) {
            <div class="flex items-start gap-2">
              <label [for]="uid + '-cc'" class="w-12 shrink-0 py-1.5 text-dimmed">Cc</label>
              <rm-address-input #ccField [value]="draft.cc" label="Cc" [inputId]="uid + '-cc'" (valueChange)="draft.cc = $event; touch()" />
            </div>
            <div class="flex items-start gap-2">
              <label [for]="uid + '-bcc'" class="w-12 shrink-0 py-1.5 text-dimmed">Bcc</label>
              <rm-address-input #bccField [value]="draft.bcc" label="Bcc" [inputId]="uid + '-bcc'" (valueChange)="draft.bcc = $event; touch()" />
            </div>
          }
          <div class="flex items-center gap-2">
            <label [for]="uid + '-subject'" class="w-12 shrink-0 text-dimmed">Subject</label>
            <input
              #subject [id]="uid + '-subject'" type="text" maxlength="998" autocomplete="off" [value]="draft.subject"
              class="min-w-0 flex-1 bg-transparent py-1.5 text-sm text-highlighted outline-none" (input)="draft.subject = subject.value; touch()"
            >
          </div>
        </div>

        @if (draft.rich) {
          <rm-rich-editor [html]="draft.html" [imageUrl]="imageUrl" label="Message" (htmlChange)="draft.html = $event; touch()" (files)="addFiles($event, true)" />
        } @else {
          <textarea
            #textarea aria-label="Message" data-testid="plain-body" [value]="draft.text"
            class="min-h-0 flex-1 resize-none bg-transparent px-3 py-2 font-sans text-sm leading-relaxed text-default outline-none"
            (input)="draft.text = textarea.value; touch()" (paste)="onPastePlain($event)"
          ></textarea>
        }

        @if (draft.attachments.length > 0) {
          <ul class="flex max-h-28 flex-wrap gap-1.5 overflow-y-auto border-t border-default px-3 py-2" aria-label="Attachments">
            @for (a of draft.attachments; track a.key) {
              <li
                class="flex max-w-full items-center gap-1.5 rounded-md border py-1 pl-2 pr-1 text-xs"
                [class]="a.state === 'failed' ? 'border-error/50 text-error' : 'border-default text-default'" data-testid="composer-attachment" [attr.data-state]="a.state"
              >
                <rm-icon
                  [name]="a.state === 'uploading' ? 'loader-circle' : a.state === 'failed' ? 'circle-alert' : a.type.startsWith('image/') ? 'image' : 'paperclip'"
                  class="size-3.5" [class]="a.state === 'uploading' ? 'animate-spin text-dimmed' : a.state === 'failed' ? '' : 'text-dimmed'"
                />
                <span class="max-w-48 truncate" [title]="a.name">{{ a.name }}</span>
                @if (a.state === 'done') {
                  <span class="shrink-0 text-dimmed">{{ fileSize(a.size) }}@if (a.inline) { · in the text}</span>
                } @else if (a.state === 'uploading') {
                  @let percent = round(a.progress * 100);
                  <span class="h-1 w-16 overflow-hidden rounded-full bg-elevated" role="progressbar" [attr.aria-label]="'Uploading ' + a.name" aria-valuemin="0" aria-valuemax="100" [attr.aria-valuenow]="percent">
                    <span class="block h-full rounded-full bg-primary" [style.width.%]="percent"></span>
                  </span>
                  <span class="shrink-0 tabular-nums text-dimmed">{{ percent }}%</span>
                } @else {
                  <span class="max-w-56 truncate" [title]="a.error">{{ a.error }}</span>
                  <button rmBtn type="button" size="xs" color="neutral" variant="link" (click)="compose.retryUpload(c, a)">Try again</button>
                }
                <button
                  type="button" class="shrink-0 rounded p-0.5 text-dimmed hover:bg-elevated hover:text-highlighted focus-visible:outline-2 focus-visible:outline-primary"
                  [attr.aria-label]="a.state === 'uploading' ? 'Cancel the upload of ' + a.name : 'Remove ' + a.name" (click)="removeAttachment(a)"
                >
                  <rm-icon name="x" class="block size-3.5" />
                </button>
              </li>
            }
          </ul>
        }

        <footer class="flex shrink-0 items-center gap-1 border-t border-default px-2 py-2 max-sm:pb-[max(0.5rem,env(safe-area-inset-bottom))]">
          <button rmBtn type="button" [disabled]="c.sending" [title]="sendTitle" data-testid="composer-send" (click)="send()">
            <rm-icon [name]="c.sending ? 'loader-circle' : 'send'" class="size-4" [class]="c.sending ? 'animate-spin' : ''" />Send
          </button>
          <input #fileInput type="file" multiple class="hidden" data-testid="composer-file" (change)="pick($event)">
          <button rmBtn type="button" color="neutral" variant="ghost" aria-label="Attach files" title="Attach files" (click)="fileInput.click()"><rm-icon name="paperclip" class="size-4" /></button>
          <button
            rmBtn type="button" color="neutral" variant="ghost" [class]="draft.rich ? '!bg-accented' : ''" aria-label="Rich text" [title]="draft.rich ? 'Switch to plain text' : 'Switch to rich text'"
            [attr.aria-pressed]="draft.rich" data-testid="composer-rich" (click)="toggleRich()"
          >
            <rm-icon name="type" class="size-4" />
          </button>
          @if (from() && identities().length <= 1) {
            <span class="ml-2 min-w-0 truncate text-xs text-dimmed" [title]="from()!.email">from {{ from()!.email }}</span>
          }
          <button
            rmBtn type="button" [color]="confirmDiscard() ? 'error' : 'neutral'" [variant]="confirmDiscard() ? 'subtle' : 'ghost'" aria-label="Discard this draft"
            [title]="confirmDiscard() ? 'Click again to discard this draft' : 'Discard'" class="ml-auto" data-testid="composer-discard" (click)="discard()"
          >
            <rm-icon name="trash-2" class="size-4" />@if (confirmDiscard()) {Discard?}
          </button>
        </footer>
      </div>

      @if (dragging() > 0 && !c.minimized) {
        <div class="pointer-events-none absolute inset-0 flex items-center justify-center rounded-t-lg border-2 border-dashed border-primary bg-default/90" aria-hidden="true">
          <span class="flex items-center gap-2 text-sm font-medium text-primary"><rm-icon name="paperclip" class="size-4" /> Drop files to attach them</span>
        </div>
      }
    </section>
  `,
})
export class ComposerPanel {
  readonly composer = input.required<Composer>()
  readonly phone = input(false)
  private readonly toaster = inject(Toaster)
  protected readonly compose = compose
  protected readonly fileSize = fileSize
  protected readonly round = Math.round
  protected readonly sendTitle = `Send (${sendKeys})`
  protected readonly uid = `composer-${Math.random().toString(36).slice(2, 8)}`

  protected readonly showCc = signal(false)
  protected readonly dragging = signal(0)
  protected readonly confirmDiscard = signal(false)
  private noSubjectOk = false

  private readonly toField = viewChild<AddressInput>('toField')
  private readonly ccField = viewChild<AddressInput>('ccField')
  private readonly bccField = viewChild<AddressInput>('bccField')
  private readonly textarea = viewChild<ElementRef<HTMLTextAreaElement>>('textarea')
  private readonly editor = viewChild(RichEditor)

  protected readonly title = computed(() => this.composer().draft.subject.trim() || names[this.composer().kind])
  protected readonly status = computed(() => {
    const c = this.composer()
    if (c.sending) return 'Sending…'
    if (c.saving) return 'Saving…'
    if (c.saveError) return 'Not saved'
    if (c.savedId && c.version === c.savedVersion) return 'Saved'
    return ''
  })
  protected readonly identities = computed(() => compose.state.identities.map((i) => ({ label: formatAddress({ name: i.name, email: i.email }), value: i.id })))
  protected readonly from = computed(() => compose.identity(this.composer().draft.identityId))

  // An inline image goes into the text once it is uploaded: the page shows
  // it from the server.
  private placed: Set<number | string> | null = null

  constructor() {
    afterNextRender(() => {
      const c = this.composer()
      this.showCc.set(c.draft.cc.length + c.draft.bcc.length > 0)
      if (this.phone() && c.kind !== 'new') return
      if (c.draft.to.length === 0) this.toField()?.focus()
      else this.focusBody()
    })
    const states = computed(() => this.composer().draft.attachments.map((a) => `${a.key}:${a.state}`).join(' '))
    effect(() => {
      states()
      untracked(() => {
        const draft = this.composer().draft
        if (!this.placed) {
          this.placed = new Set(draft.attachments.map((a) => a.key))
          return
        }
        for (const a of draft.attachments) {
          if (!a.inline || a.state !== 'done' || this.placed.has(a.key)) continue
          this.placed.add(a.key)
          const url = this.imageUrl(a.cid)
          if (url && draft.rich) this.editor()?.insertImage(a.cid, url)
        }
      })
    })
  }

  protected max0(n: number): number {
    return Math.max(0, n)
  }

  protected touch(): void {
    compose.touch(this.composer())
  }

  protected openCc(): void {
    this.showCc.set(true)
    setTimeout(() => this.ccField()?.focus())
  }

  private focusBody(): void {
    if (this.composer().draft.rich) this.editor()?.focus()
    else {
      const t = this.textarea()?.nativeElement
      if (!t) return
      t.focus()
      t.setSelectionRange(0, 0)
      t.scrollTop = 0
    }
  }

  // --- attachments ---

  protected readonly imageUrl = (cid: string): string | null => {
    const a = this.composer().draft.attachments.find((x) => x.cid === cid)
    return a?.blobId ? client.downloadUrl(a.blobId, a.name, a.type) : null
  }

  protected addFiles(files: File[], inline: boolean): void {
    if (files.length === 0) return
    const { refused } = compose.attach(this.composer(), files.map((file) => ({ file, name: file.name || 'file' })), inline)
    for (const r of refused) this.toaster.error(r)
  }

  protected removeAttachment(a: Attachment): void {
    compose.removeAttachment(this.composer(), a)
    if (a.inline) this.editor()?.removeImage(a.cid)
  }

  protected pick(e: Event): void {
    const el = e.target as HTMLInputElement
    this.addFiles([...(el.files ?? [])], false)
    el.value = ''
  }

  protected onPastePlain(e: ClipboardEvent): void {
    const files = [...(e.clipboardData?.files ?? [])]
    if (files.length === 0) return
    e.preventDefault()
    this.addFiles(files, false)
  }

  protected hasFiles(e: DragEvent): boolean {
    return [...(e.dataTransfer?.types ?? [])].includes('Files')
  }
  protected onDrop(e: DragEvent): void {
    this.dragging.set(0)
    if (!this.hasFiles(e)) return
    e.preventDefault()
    this.addFiles([...(e.dataTransfer?.files ?? [])], false)
  }

  // --- the body ---

  protected toggleRich(): void {
    const c = this.composer()
    compose.setRich(c, !c.draft.rich)
    try {
      localStorage.setItem('robinmail-rich', c.draft.rich ? '1' : '0')
    } catch {
      // Storage is off: the choice holds for this visit.
    }
    setTimeout(() => this.focusBody())
  }

  // --- send, close, discard ---

  protected async send(): Promise<void> {
    const c = this.composer()
    if (c.sending) return
    const fields = [this.toField(), this.ccField(), this.bccField()]
    if (!fields.map((f) => f?.commit() ?? true).every(Boolean)) {
      c.sendError = 'A recipient field holds something that is not an email address.'
      return
    }
    await Promise.resolve()
    if (c.draft.subject.trim() === '' && !this.noSubjectOk && compose.problems(c).length === 0) {
      this.noSubjectOk = true
      c.sendError = 'This message has no subject. Send again to send it as it is.'
      return
    }
    // The dock shows the notice with "undo": this panel is gone by then.
    await compose.send(c)
  }

  protected async close(): Promise<void> {
    await compose.close(this.composer())
  }

  protected discard(): void {
    const c = this.composer()
    if (!this.confirmDiscard() && (c.savedId || c.version > 0)) {
      this.confirmDiscard.set(true)
      setTimeout(() => this.confirmDiscard.set(false), 4000)
      return
    }
    void compose.discard(c)
  }

  protected onKey(e: KeyboardEvent): void {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault()
      void this.send()
    }
  }
}
