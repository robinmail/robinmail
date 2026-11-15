// Identities: the addresses to send as. The account's own address is
// always there; others are that address with a +tag, or an alias that
// delivers to the account (phase 8), each with its own name, reply-to and
// signature.

import { ChangeDetectionStrategy, Component, type OnInit, computed, signal } from '@angular/core'
import { FormsModule } from '@angular/forms'
import { type Identity, authApi, formatAddress, isValidEmail, parseAddresses } from '@robinmail/jmap'
import { auth, compose, loadAccount } from '../app'
import { SettingsShell } from '../components/settings-shell'
import { ALERT, Btn, Input, LABEL, SKELETON } from '../ui/controls'
import { Icon } from '../ui/icon'
import { Modal } from '../ui/modal'
import { Select } from '../ui/select'

type Dialog = { kind: 'add' } | { kind: 'edit'; identity: Identity } | { kind: 'remove'; identity: Identity }

@Component({
  selector: 'rm-settings-page',
  imports: [FormsModule, SettingsShell, Btn, Input, Icon, Modal, Select],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'block h-full overflow-y-auto' },
  template: `
    <rm-settings-shell title="Identities">
      @if (loadError()) {
        <div class="flex flex-col items-center gap-3 pt-16 text-center">
          <rm-icon name="cloud-off" class="size-10 text-dimmed" />
          <p class="text-sm text-muted">The identities could not be loaded.<br>{{ loadError() }}</p>
          <button rmBtn type="button" (click)="load()">Try again</button>
        </div>
      } @else {
        <p class="text-sm text-muted">
          An identity is an address you send from, with a name and a signature. Besides your own address you can use it
          with a tag, like <span class="whitespace-nowrap font-medium text-default">{{ base().local }}+shop&#64;{{ base().domain }}</span>: mail to a tagged address
          arrives in this account too.
        </p>

        @if (!compose.state.identitiesLoaded) {
          <div class="flex flex-col gap-3" aria-busy="true">
            <div class="h-24 w-full ${SKELETON}"></div>
            <div class="h-24 w-full ${SKELETON}"></div>
          </div>
        } @else {
          <ul class="flex flex-col gap-3" aria-label="Identities">
            @for (i of compose.state.identities; track i.id) {
              <li class="rounded-lg border border-default p-4" data-testid="identity">
                <div class="flex items-start gap-2">
                  <div class="min-w-0 flex-1">
                    <p class="truncate font-medium text-highlighted">{{ i.name || '(no name)' }}</p>
                    <p class="truncate text-sm text-muted">{{ i.email }}</p>
                  </div>
                  <button rmBtn type="button" size="sm" color="neutral" variant="ghost" [attr.aria-label]="'Edit ' + i.email" (click)="show({ kind: 'edit', identity: i })">
                    <rm-icon name="pencil" class="size-4" />Edit
                  </button>
                  @if (i.mayDelete) {
                    <button rmBtn type="button" size="sm" color="neutral" variant="ghost" [attr.aria-label]="'Remove ' + i.email" (click)="show({ kind: 'remove', identity: i })">
                      <rm-icon name="trash-2" class="size-4" />
                    </button>
                  }
                </div>
                <dl class="mt-2 grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-sm">
                  @if (i.replyTo?.length) {
                    <dt class="text-dimmed">Reply-to</dt>
                    <dd class="min-w-0 break-words text-default">{{ list(i.replyTo) }}</dd>
                  }
                  <dt class="text-dimmed">Signature</dt>
                  <dd class="min-w-0 whitespace-pre-wrap break-words" [class]="i.textSignature ? 'text-default' : 'text-dimmed'">{{ i.textSignature || 'None' }}</dd>
                </dl>
              </li>
            }
          </ul>
        }
        <div>
          <button rmBtn type="button" color="neutral" variant="subtle" [disabled]="!compose.state.identitiesLoaded" data-testid="add-identity" (click)="show({ kind: 'add' })">
            <rm-icon name="plus" class="size-4" />Add an identity
          </button>
        </div>
      }
    </rm-settings-shell>

    @let d = dialog();
    <rm-modal
      [open]="d !== null" (openChange)="$event || dialog.set(null)" [title]="title()"
      [description]="d?.kind === 'remove' ? 'Mail sent to this address still arrives.' : 'How your mail shows to the people you write to.'"
    >
      <form id="identity-form" class="flex flex-col gap-4" (ngSubmit)="submit()">
        @if (error()) {
          <div class="${ALERT}" role="alert"><rm-icon name="circle-alert" class="mt-0.5 size-4" /><span>{{ error() }}</span></div>
        }
        @if (d?.kind === 'remove') {
          <p class="text-sm">You will no longer be able to send from this address. Messages you sent from it stay.</p>
        } @else {
          @if (d?.kind === 'add' && bases().length > 1) {
            <div class="flex flex-col gap-1">
              <label for="identity-base" class="text-sm font-medium text-default">Send as</label>
              <rm-select buttonId="identity-base" testid="identity-base" [value]="base().address" [options]="baseOptions()" (valueChange)="baseAddress.set($event)" />
              <p class="text-sm text-muted">Your own address, or an alias that delivers to you.</p>
            </div>
          }
          @if (d?.kind === 'add') {
            <div class="flex flex-col gap-1">
              <label for="identity-tag" class="text-sm font-medium text-default">Address</label>
              <div class="flex items-center gap-1 text-sm">
                <span class="shrink-0 text-muted">{{ base().local }}+</span>
                <input
                  rmInput id="identity-tag" class="min-w-0 flex-1" name="tag" autofocus autocomplete="off" autocapitalize="off" spellcheck="false" maxlength="40"
                  [placeholder]="tagNeeded() ? 'tag' : 'tag (optional)'" [(ngModel)]="tag"
                >
                <span class="min-w-0 truncate text-muted">&#64;{{ base().domain }}</span>
              </div>
              <p class="text-sm text-muted">{{ newAddress() }}</p>
            </div>
          }
          <div class="flex flex-col gap-1">
            <label class="${LABEL}"><span>Name</span><input rmInput name="name" maxlength="200" autocomplete="name" [(ngModel)]="name"></label>
            <p class="text-sm text-muted">Shown next to the address.</p>
          </div>
          <div class="flex flex-col gap-1">
            <label class="${LABEL}"><span>Reply-to</span><input rmInput name="replyTo" autocomplete="off" inputmode="email" [(ngModel)]="replyTo"></label>
            <p class="text-sm text-muted">Where answers should go, if not to this address. Optional.</p>
          </div>
          <div class="flex flex-col gap-1">
            <label class="${LABEL}"><span>Signature</span><textarea rmInput name="signature" rows="4" [(ngModel)]="signature"></textarea></label>
            <p class="text-sm text-muted">Plain text, put under a new message.</p>
          </div>
        }
      </form>
      <ng-container modal-footer>
        <button rmBtn type="button" variant="ghost" color="neutral" (click)="dialog.set(null)">Cancel</button>
        <button rmBtn type="submit" form="identity-form" [color]="d?.kind === 'remove' ? 'error' : 'primary'" [disabled]="busy()">
          {{ d?.kind === 'remove' ? 'Remove' : d?.kind === 'edit' ? 'Save' : 'Add' }}
        </button>
      </ng-container>
    </rm-modal>
  `,
})
export class SettingsPage implements OnInit {
  protected readonly compose = compose
  protected readonly loadError = signal('')
  private readonly sendAs = signal<string[]>([])

  // The addresses a new identity can be for: the account's own, and the
  // aliases that deliver to it. The chosen one stands around the tag.
  private readonly own = computed(() => compose.state.identities.find((i) => !i.mayDelete)?.email ?? auth.account?.username ?? '')
  protected readonly baseAddress = signal('')
  protected readonly bases = computed(() => (this.sendAs().length ? this.sendAs() : [this.own()]))
  protected readonly baseOptions = computed(() => this.bases().map((b) => ({ label: b, value: b })))
  protected readonly base = computed(() => {
    const chosen = this.bases().includes(this.baseAddress()) ? this.baseAddress() : this.own()
    const at = chosen.lastIndexOf('@')
    return { address: chosen, local: chosen.slice(0, at), domain: chosen.slice(at + 1) }
  })
  // An alias can be used as it is; the account's own address has its
  // identity already, so a new one for it needs a tag.
  protected readonly tagNeeded = computed(() => compose.state.identities.some((i) => i.email === this.base().address))

  protected readonly dialog = signal<Dialog | null>(null)
  protected readonly tag = signal('')
  protected readonly name = signal('')
  protected readonly replyTo = signal('')
  protected readonly signature = signal('')
  protected readonly busy = signal(false)
  protected readonly error = signal('')

  protected readonly title = computed(() => {
    const d = this.dialog()
    if (!d) return ''
    return d.kind === 'add' ? 'New identity' : d.kind === 'edit' ? `Edit ${d.identity.email}` : `Remove ${d.identity.email}?`
  })
  protected readonly newAddress = computed(() => (this.tag().trim() ? `${this.base().local}+${this.tag().trim()}@${this.base().domain}` : this.base().address))

  ngOnInit(): void {
    document.title = 'Identities · robinmail'
    void this.load()
  }

  protected async load(): Promise<void> {
    this.loadError.set('')
    try {
      await loadAccount()
    } catch (e) {
      if (auth.account) this.loadError.set(e instanceof Error ? e.message : String(e))
    }
    // Without the list the page still works for the account's own address.
    try {
      this.sendAs.set(await authApi.addresses())
    } catch {
      this.sendAs.set([])
    }
  }

  protected list(addresses: Identity['replyTo']): string {
    return (addresses ?? []).map(formatAddress).join(', ')
  }

  protected show(d: Dialog): void {
    this.dialog.set(d)
    this.baseAddress.set(this.own())
    this.error.set('')
    this.tag.set('')
    if (d.kind === 'edit') {
      this.name.set(d.identity.name)
      this.replyTo.set(this.list(d.identity.replyTo))
      this.signature.set(d.identity.textSignature)
    } else {
      this.name.set(compose.state.identities[0]?.name ?? '')
      this.replyTo.set('')
      this.signature.set('')
    }
  }

  protected async submit(): Promise<void> {
    const d = this.dialog()
    if (!d || this.busy()) return
    this.error.set('')
    const replyTo = parseAddresses(this.replyTo())
    if (d.kind !== 'remove') {
      if (replyTo.invalid.length) {
        this.error.set(`Reply-to: “${replyTo.invalid[0]}” is not an email address.`)
        return
      }
      if (d.kind === 'add' && ((this.tag().trim() === '' && this.tagNeeded()) || !isValidEmail(this.newAddress()))) {
        this.error.set('Give the address a tag of letters, digits, dots or dashes.')
        return
      }
    }
    const input = { name: this.name().trim(), replyTo: replyTo.addresses, textSignature: this.signature().replace(/\s+$/, '') }
    this.busy.set(true)
    try {
      if (d.kind === 'add') await compose.createIdentity(this.newAddress(), input)
      else if (d.kind === 'edit') await compose.updateIdentity(d.identity.id, input)
      else await compose.destroyIdentity(d.identity.id)
      this.dialog.set(null)
    } catch (e) {
      this.error.set(e instanceof Error ? e.message : String(e))
    } finally {
      this.busy.set(false)
    }
  }
}
