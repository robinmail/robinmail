// Contacts: the cards of the address books the user sees, a search over
// them, and an editor. Import takes a .vcf file; each address book can be
// downloaded as one.

import { ChangeDetectionStrategy, Component, type OnInit, computed, inject, signal } from '@angular/core'
import { FormsModule } from '@angular/forms'
import { RouterLink } from '@angular/router'
import { type BookView, type CardView, type ContactDraft, draftOf } from '@robinmail/jmap'
import { compose, contacts } from '../app'
import { AppSwitcher } from '../components/app-switcher'
import { ALERT, Btn, Input, LABEL, SKELETON } from '../ui/controls'
import { Icon } from '../ui/icon'
import { Modal } from '../ui/modal'
import { Toaster } from '../ui/toast'

@Component({
  selector: 'rm-contacts-page',
  imports: [FormsModule, RouterLink, AppSwitcher, Btn, Input, Icon, Modal],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'block h-full' },
  template: `
    <div class="flex h-full overflow-hidden">
      <aside class="hidden w-64 shrink-0 flex-col border-r border-default bg-muted lg:flex">
        <div class="flex h-(--ui-header-height) shrink-0 items-center px-4">
          <a routerLink="/mail" class="flex items-center gap-2 rounded focus-visible:outline-2 focus-visible:outline-primary">
            <img src="/favicon.svg" alt="" class="size-6">
            <span class="font-semibold tracking-tight text-highlighted">robinmail</span>
          </a>
        </div>
        <div class="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-3 pb-3">
          <rm-app-switcher />
          <button rmBtn type="button" [block]="true" data-testid="new-contact" [disabled]="!store.state.ready" (click)="newContact()">
            <rm-icon name="plus" class="size-4" />New contact
          </button>
          <section aria-label="Address books">
            <h2 class="mb-1 px-1 text-xs font-medium uppercase tracking-wide text-dimmed">Address books</h2>
            <ul class="flex flex-col gap-0.5 text-sm">
              <li>
                <button type="button" class="w-full rounded-md px-2 py-1 text-left" [class]="book() === null ? 'bg-elevated font-medium text-highlighted' : 'text-muted hover:bg-elevated/50'" (click)="book.set(null)">All contacts</button>
              </li>
              @for (b of store.state.books; track b.accountId + b.book.id) {
                <li class="group flex items-center">
                  <button type="button" class="min-w-0 flex-1 truncate rounded-md px-2 py-1 text-left" data-testid="book-item"
                    [class]="book() === b ? 'bg-elevated font-medium text-highlighted' : 'text-muted hover:bg-elevated/50'" (click)="book.set(b)">
                    {{ b.book.name }}@if (!b.own) {<span class="text-dimmed"> · {{ b.owner }}</span>}
                  </button>
                  <a class="rounded p-1 text-dimmed hover:text-highlighted lg:opacity-0 lg:group-hover:opacity-100 focus-visible:opacity-100" [href]="store.exportURL(b)" download
                    [attr.aria-label]="'Download ' + b.book.name" title="Download as .vcf"><rm-icon name="download" class="size-3.5" /></a>
                </li>
              }
            </ul>
          </section>
          <label rmBtn variant="ghost" color="neutral" size="sm" class="cursor-pointer justify-start !text-muted" title="Import a .vcf file">
            <rm-icon name="upload" class="size-4" />Import .vcf
            <input type="file" accept=".vcf,text/vcard" class="sr-only" data-testid="import-vcf" (change)="importFile($event)">
          </label>
        </div>
      </aside>

      <section class="flex min-w-0 flex-col border-r border-default max-md:w-full md:w-80 md:shrink-0" [class]="selected() ? 'max-md:hidden' : ''">
        <div class="flex h-(--ui-header-height) shrink-0 items-center gap-2 border-b border-default px-3">
          <a routerLink="/mail" rmBtn variant="ghost" color="neutral" size="sm" class="lg:hidden" aria-label="Back to mail"><rm-icon name="arrow-left" class="size-4" /></a>
          <input rmInput type="search" placeholder="Search contacts" aria-label="Search contacts" data-testid="contact-search" [ngModel]="query()" (ngModelChange)="query.set($event)">
          <button rmBtn type="button" size="sm" class="lg:hidden" aria-label="New contact" (click)="newContact()"><rm-icon name="plus" class="size-4" /></button>
        </div>
        @if (!store.state.ready) {
          <div class="flex flex-col gap-2 p-3" aria-busy="true">
            @for (n of [1, 2, 3, 4]; track n) { <div class="h-10 ${SKELETON}"></div> }
          </div>
        } @else if (list().length === 0) {
          <p class="p-6 text-center text-sm text-muted">{{ query() ? 'No contact matches.' : 'No contacts yet.' }}</p>
        }
        <ul class="min-h-0 flex-1 overflow-y-auto" aria-label="Contacts">
          @for (v of list(); track v.accountId + v.card.id) {
            <li>
              <button type="button" class="flex w-full items-center gap-3 border-b border-default/60 px-3 py-2 text-left hover:bg-elevated/50" data-testid="contact-row"
                [class.bg-elevated]="selected() === v" (click)="select(v)">
                <span class="inline-flex size-8 shrink-0 items-center justify-center rounded-full bg-primary/15 text-xs font-semibold text-primary">{{ initials(v.name) }}</span>
                <span class="min-w-0">
                  <span class="block truncate text-sm font-medium text-highlighted">{{ v.name }}</span>
                  <span class="block truncate text-xs text-muted">{{ firstEmail(v) || firstPhone(v) }}</span>
                </span>
              </button>
            </li>
          }
        </ul>
      </section>

      <main class="min-w-0 flex-1 overflow-y-auto" [class]="selected() ? '' : 'max-md:hidden'">
        @let v = selected();
        @if (v) {
          <div class="mx-auto flex max-w-2xl flex-col gap-4 p-6" data-testid="contact-detail">
            <button rmBtn type="button" variant="ghost" color="neutral" size="sm" class="self-start md:hidden" (click)="selected.set(null)"><rm-icon name="arrow-left" class="size-4" />Contacts</button>
            <div class="flex items-center gap-4">
              <span class="inline-flex size-14 items-center justify-center rounded-full bg-primary/15 text-lg font-semibold text-primary">{{ initials(v.name) }}</span>
              <div class="min-w-0 flex-1">
                <h1 class="truncate text-xl font-semibold text-highlighted">{{ v.name }}</h1>
                @if (org(v)) { <p class="text-sm text-muted">{{ org(v) }}</p> }
              </div>
              @if (writable(v)) {
                <button rmBtn type="button" variant="outline" color="neutral" data-testid="contact-edit" (click)="edit(v)"><rm-icon name="pencil" class="size-4" />Edit</button>
              }
            </div>
            <dl class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm">
              @for (e of emails(v); track e) {
                <dt class="text-muted">Email</dt>
                <dd class="flex items-center gap-2"><a class="text-primary hover:underline" [href]="'mailto:' + e">{{ e }}</a>
                  <button rmBtn type="button" size="xs" variant="ghost" color="neutral" [attr.aria-label]="'Write to ' + e" (click)="write(v, e)"><rm-icon name="pen-line" class="size-3.5" /></button>
                </dd>
              }
              @for (p of phones(v); track p) {
                <dt class="text-muted">Phone</dt><dd><a class="hover:underline" [href]="'tel:' + p">{{ p }}</a></dd>
              }
              @if (draft(v).address) { <dt class="text-muted">Address</dt><dd class="whitespace-pre-wrap">{{ draft(v).address }}</dd> }
              @if (draft(v).birthday) { <dt class="text-muted">Birthday</dt><dd>{{ draft(v).birthday }}</dd> }
              @if (draft(v).note) { <dt class="text-muted">Notes</dt><dd class="whitespace-pre-wrap">{{ draft(v).note }}</dd> }
              <dt class="text-muted">Address book</dt><dd>{{ store.book(v.accountId, v.bookId)?.book?.name }}</dd>
            </dl>
          </div>
        } @else {
          <div class="flex h-full flex-col items-center justify-center gap-3 text-dimmed">
            <rm-icon name="contact" class="size-16" />
            <p class="text-sm">Choose a contact.</p>
          </div>
        }
      </main>
    </div>

    <rm-modal [open]="editing() !== null" (openChange)="$event || editing.set(null)" [wide]="true" [title]="editingCard() ? 'Edit contact' : 'New contact'">
      @let d = editing();
      @if (d) {
        <form id="contact-form" class="flex flex-col gap-4" (ngSubmit)="save()">
          @if (error()) {
            <div class="${ALERT}" role="alert"><rm-icon name="circle-alert" class="mt-0.5 size-4" /><span>{{ error() }}</span></div>
          }
          <div class="grid gap-4 sm:grid-cols-2">
            <label class="${LABEL}"><span>First name</span><input rmInput name="given" autofocus data-testid="contact-given" [(ngModel)]="d.given"></label>
            <label class="${LABEL}"><span>Last name</span><input rmInput name="surname" data-testid="contact-surname" [(ngModel)]="d.surname"></label>
            <label class="${LABEL}"><span>Organization</span><input rmInput name="org" data-testid="contact-org" [(ngModel)]="d.organization"></label>
            <label class="${LABEL}"><span>Job title</span><input rmInput name="title" [(ngModel)]="d.title"></label>
          </div>
          <fieldset class="flex flex-col gap-2">
            <legend class="mb-1 text-sm font-medium">Email</legend>
            @for (e of d.emails; track $index) {
              <div class="flex gap-2">
                <input rmInput type="email" [name]="'email' + $index" [attr.data-testid]="'contact-email-' + $index" [(ngModel)]="e.address" aria-label="Email address">
                <select rmInput class="!w-28" [name]="'emailctx' + $index" [(ngModel)]="e.context" aria-label="Kind"><option value="">Other</option><option value="work">Work</option><option value="private">Home</option></select>
              </div>
            }
            <button rmBtn type="button" variant="link" color="neutral" class="self-start" (click)="d.emails.push({ address: '', context: '' })">Add an address</button>
          </fieldset>
          <fieldset class="flex flex-col gap-2">
            <legend class="mb-1 text-sm font-medium">Phone</legend>
            @for (p of d.phones; track $index) {
              <div class="flex gap-2">
                <input rmInput type="tel" [name]="'phone' + $index" [attr.data-testid]="'contact-phone-' + $index" [(ngModel)]="p.number" aria-label="Phone number">
                <select rmInput class="!w-28" [name]="'phonekind' + $index" [(ngModel)]="p.kind" aria-label="Kind"><option value="">Other</option><option value="mobile">Mobile</option><option value="work">Work</option><option value="home">Home</option></select>
              </div>
            }
            <button rmBtn type="button" variant="link" color="neutral" class="self-start" (click)="d.phones.push({ number: '', kind: '' })">Add a number</button>
          </fieldset>
          <label class="${LABEL}"><span>Address</span><textarea rmInput name="address" rows="2" [(ngModel)]="d.address"></textarea></label>
          <div class="grid gap-4 sm:grid-cols-2">
            <label class="${LABEL}"><span>Birthday (yyyy-mm-dd)</span><input rmInput name="birthday" placeholder="1990-04-15" [(ngModel)]="d.birthday"></label>
            <label class="${LABEL}"><span>Address book</span>
              <select rmInput name="book" [ngModel]="d.accountId + '/' + d.bookId" (ngModelChange)="setBook(d, $event)">
                @for (b of writableBooks(); track b.accountId + b.book.id) {
                  <option [value]="b.accountId + '/' + b.book.id">{{ b.book.name }}{{ b.own ? '' : ' · ' + b.owner }}</option>
                }
              </select>
            </label>
          </div>
          <label class="${LABEL}"><span>Notes</span><textarea rmInput name="note" rows="3" [(ngModel)]="d.note"></textarea></label>
        </form>
      }
      <ng-container modal-footer>
        @if (editingCard()) {
          <button rmBtn type="button" variant="ghost" color="error" class="mr-auto" data-testid="contact-delete" [disabled]="busy()" (click)="remove()"><rm-icon name="trash-2" class="size-4" />Delete</button>
        }
        <button rmBtn type="button" variant="ghost" color="neutral" (click)="editing.set(null)">Cancel</button>
        <button rmBtn type="submit" form="contact-form" data-testid="contact-save" [disabled]="busy()">Save</button>
      </ng-container>
    </rm-modal>
  `,
})
export class ContactsPage implements OnInit {
  protected readonly store = contacts
  private readonly toaster = inject(Toaster)
  protected readonly query = signal('')
  protected readonly book = signal<BookView | null>(null)
  protected readonly selected = signal<CardView | null>(null)
  protected readonly editing = signal<ContactDraft | null>(null)
  protected readonly editingCard = signal<CardView | null>(null)
  protected readonly busy = signal(false)
  protected readonly error = signal('')

  protected readonly list = computed(() => {
    const b = this.book()
    return this.store.search(this.query()).filter((v) => !b || (v.accountId === b.accountId && v.bookId === b.book.id))
  })
  protected readonly writableBooks = computed(() => this.store.state.books.filter((b) => b.book.myRights.mayWrite))

  ngOnInit(): void {
    this.store.load().catch((e) => (this.store.state.error = (e as Error).message))
  }

  protected initials(name: string): string {
    return name.split(/\s+/).filter(Boolean).slice(0, 2).map((w) => w[0]!.toUpperCase()).join('') || '?'
  }
  protected firstEmail(v: CardView): string {
    return Object.values(v.card.emails ?? {})[0]?.address ?? ''
  }
  protected firstPhone(v: CardView): string {
    return Object.values(v.card.phones ?? {})[0]?.number ?? ''
  }
  protected emails(v: CardView): string[] {
    return Object.values(v.card.emails ?? {}).map((e) => e.address)
  }
  protected phones(v: CardView): string[] {
    return Object.values(v.card.phones ?? {}).map((p) => p.number)
  }
  protected org(v: CardView): string {
    const d = draftOf(v)
    return [d.title, d.organization].filter(Boolean).join(', ')
  }
  protected draft(v: CardView): ContactDraft {
    return draftOf(v)
  }
  protected writable(v: CardView): boolean {
    return !!this.store.book(v.accountId, v.bookId)?.book.myRights.mayWrite
  }

  protected select(v: CardView): void {
    this.selected.set(v)
  }

  protected write(v: CardView, email: string): void {
    compose.compose({ to: [{ name: v.name, email }], cc: [], bcc: [], subject: '', body: '' })
  }

  protected newContact(): void {
    const b = this.book() && this.book()!.book.myRights.mayWrite ? this.book()! : this.writableBooks().find((x) => x.own && x.book.isDefault) ?? this.writableBooks()[0]
    if (!b) return
    this.error.set('')
    this.editingCard.set(null)
    this.editing.set({ accountId: b.accountId, bookId: b.book.id, given: '', surname: '', organization: '', title: '', emails: [{ address: '', context: '' }],
      phones: [{ number: '', kind: '' }], address: '', birthday: '', note: '' })
  }

  protected edit(v: CardView): void {
    this.error.set('')
    const d = draftOf(v)
    if (!d.emails.length) d.emails.push({ address: '', context: '' })
    if (!d.phones.length) d.phones.push({ number: '', kind: '' })
    this.editingCard.set(v)
    this.editing.set(d)
  }

  protected setBook(d: ContactDraft, key: string): void {
    const [a, b] = key.split('/')
    d.accountId = a!
    d.bookId = b!
  }

  protected async save(): Promise<void> {
    const d = this.editing()
    if (!d || this.busy()) return
    if (!d.given.trim() && !d.surname.trim() && !d.organization.trim() && !d.emails.some((e) => e.address.trim())) {
      this.error.set('Give the contact a name, an organization or an address.')
      return
    }
    this.busy.set(true)
    try {
      const id = await this.store.save(d, this.editingCard() ?? undefined)
      this.editing.set(null)
      this.selected.set(this.store.state.cards.find((c) => c.card.id === id && c.accountId === d.accountId) ?? null)
      this.toaster.add({ title: 'Saved.' })
    } catch (err) {
      this.error.set((err as Error).message)
    } finally {
      this.busy.set(false)
    }
  }

  protected async remove(): Promise<void> {
    const v = this.editingCard()
    if (!v || this.busy() || !confirm(`Delete ${v.name}?`)) return
    this.busy.set(true)
    try {
      await this.store.remove(v)
      this.editing.set(null)
      this.selected.set(null)
    } catch (err) {
      this.error.set((err as Error).message)
    } finally {
      this.busy.set(false)
    }
  }

  protected async importFile(ev: Event): Promise<void> {
    const input = ev.target as HTMLInputElement
    const file = input.files?.[0]
    input.value = ''
    if (!file) return
    if (!this.store.state.ready) await this.store.load()
    const b = this.book() ?? this.writableBooks().find((x) => x.own && x.book.isDefault) ?? this.writableBooks()[0]
    if (!b) {
      this.toaster.error('There is no address book to import into.')
      return
    }
    try {
      const n = await this.store.importVCF(await file.text(), b)
      this.toaster.add({ title: `${n} ${n === 1 ? 'contact' : 'contacts'} imported into ${b.book.name}.` })
    } catch (err) {
      this.toaster.error((err as Error).message)
    }
  }
}
