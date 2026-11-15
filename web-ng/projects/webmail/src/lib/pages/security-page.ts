// Password and security: the password, app passwords for mail programs,
// two-step login, and the browsers that are logged in.

import { ChangeDetectionStrategy, Component, type OnInit, computed, inject, signal } from '@angular/core'
import { FormsModule } from '@angular/forms'
import { Router } from '@angular/router'
import { authApi as api } from '@robinmail/jmap'
import { auth, compose, store } from '../app'
import { SettingsShell } from '../components/settings-shell'
import { ALERT, Badge, Btn, Input, LABEL, SKELETON } from '../ui/controls'
import { Icon } from '../ui/icon'
import { Modal } from '../ui/modal'
import { Toaster } from '../ui/toast'
import { fullDate, shortDate } from '../util/format'

// One dialog at a time. Each has its own little form.
type Dialog =
  | { kind: 'new-app' }
  | { kind: 'app-secret'; app: api.AppPassword }
  | { kind: 'revoke-app'; app: api.AppPassword }
  | { kind: 'totp-password' }
  | { kind: 'totp-scan'; enrolment: api.Enrolment }
  | { kind: 'codes'; codes: string[]; fresh: boolean }
  | { kind: 'totp-off' }
  | { kind: 'new-codes' }
  | { kind: 'revoke-session'; session: api.WebSession }
  | { kind: 'revoke-others' }

const titles: Record<Dialog['kind'], string> = {
  'new-app': 'New app password',
  'app-secret': 'Your new app password',
  'revoke-app': 'Revoke this app password?',
  'totp-password': 'Turn on two-step login',
  'totp-scan': 'Scan the code',
  'codes': 'Recovery codes',
  'totp-off': 'Turn off two-step login',
  'new-codes': 'New recovery codes',
  'revoke-session': 'Log this browser out?',
  'revoke-others': 'Log out everywhere else?',
}
const submitLabels: Partial<Record<Dialog['kind'], string>> = {
  'new-app': 'Make the password', 'revoke-app': 'Revoke', 'totp-password': 'Continue', 'totp-scan': 'Turn on', 'totp-off': 'Turn off', 'new-codes': 'Make new codes',
}
const scopeNames: Record<string, string> = { imap: 'reading (IMAP)', smtp: 'sending (SMTP)', jmap: 'JMAP', dav: 'calendars and contacts (CalDAV, CardDAV)' }
const NOTE = 'flex items-start gap-2 rounded-md px-3 py-2 text-sm ring ring-inset'

@Component({
  selector: 'rm-security-page',
  imports: [FormsModule, SettingsShell, Btn, Badge, Input, Icon, Modal],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'block h-full overflow-y-auto' },
  template: `
    <rm-settings-shell title="Security">
      @if (loadError()) {
        <div class="flex flex-col items-center gap-3 pt-16 text-center">
          <rm-icon name="cloud-off" class="size-10 text-dimmed" />
          <p class="text-sm text-muted">{{ loadError() }}</p>
          <button rmBtn type="button" (click)="load()">Try again</button>
        </div>
      } @else if (!loaded()) {
        <div class="flex flex-col gap-3" aria-busy="true">
          @for (n of [1, 2, 3, 4]; track n) {
            <div class="h-28 w-full ${SKELETON}"></div>
          }
        </div>
      } @else {
        <div class="flex flex-col gap-8">
          <!-- Password -->
          <section aria-labelledby="h-password" class="flex flex-col gap-3" data-testid="section-password">
            <h2 id="h-password" class="text-base font-semibold text-highlighted">Password</h2>
            @if (!hasPassword()) {
              <p class="text-sm text-muted">This account has no password: you log in through your provider. To use a mail program, make an app password below.</p>
            } @else {
              <form class="flex flex-col gap-3" data-testid="password-form" (ngSubmit)="changePassword()">
                <p class="text-sm text-muted">The password of your account opens the webmail. Changing it logs every other browser out.</p>
                @if (pwError()) {
                  <div class="${ALERT}" role="alert"><rm-icon name="circle-alert" class="mt-0.5 size-4" /><span>{{ pwError() }}</span></div>
                }
                <label class="${LABEL}"><span>Current password</span><input rmInput type="password" name="current" autocomplete="current-password" required [(ngModel)]="pwCurrent"></label>
                <div class="flex flex-col gap-1">
                  <label class="${LABEL}"><span>New password</span><input rmInput type="password" name="new" autocomplete="new-password" minlength="8" required [(ngModel)]="pwNext"></label>
                  <p class="text-sm text-muted">At least 8 characters.</p>
                </div>
                <label class="${LABEL}"><span>New password, again</span><input rmInput type="password" name="again" autocomplete="new-password" minlength="8" required [(ngModel)]="pwAgain"></label>
                <div><button rmBtn type="submit" [disabled]="pwBusy()">Change password</button></div>
              </form>
            }
          </section>

          <!-- App passwords -->
          <section aria-labelledby="h-apps" class="flex flex-col gap-3" data-testid="section-apps">
            <h2 id="h-apps" class="text-base font-semibold text-highlighted">App passwords</h2>
            <p class="text-sm text-muted">
              A mail program such as Thunderbird or the Mail app on a phone cannot do two-step login or log in through a provider.
              It gets a password of its own instead: one per device, made here, which you can take away again without touching the others.
            </p>
            @if (passwordOpensMail() && hasPassword()) {
              <div class="${NOTE} bg-elevated ring-accented" data-testid="password-opens-mail">
                <rm-icon name="info" class="mt-0.5 size-4 text-muted" />
                <div><p class="font-medium text-highlighted">Your account password still works in mail programs.</p>
                  <p class="text-muted">It stops working there once you make an app password or turn on two-step login. From then on it only opens the webmail.</p></div>
              </div>
            } @else {
              <div class="${NOTE} bg-elevated ring-accented" data-testid="password-closed-for-mail">
                <rm-icon name="lock" class="mt-0.5 size-4 text-muted" />
                <div><p class="font-medium text-highlighted">Mail programs need an app password.</p><p class="text-muted">Your account password only opens the webmail.</p></div>
              </div>
            }
            @if (appPasswords().length) {
              <ul class="flex flex-col gap-2" aria-label="App passwords">
                @for (a of appPasswords(); track a.id) {
                  <li class="flex items-start gap-3 rounded-lg border border-default p-3" data-testid="app-password">
                    <rm-icon name="key-round" class="mt-0.5 size-5 text-dimmed" />
                    <div class="min-w-0 flex-1 text-sm">
                      <p class="truncate font-medium text-highlighted">{{ a.name }}</p>
                      <p class="text-muted">For {{ scopeText(a) }}. Made {{ shortDate(a.createdAt) }}.</p>
                      <p class="text-muted" [title]="a.usedAt ? fullDate(a.usedAt) : ''">{{ a.usedAt ? 'Last used ' + shortDate(a.usedAt) + ' from ' + a.usedIp : 'Never used' }}</p>
                    </div>
                    <button rmBtn type="button" size="sm" color="error" variant="ghost" [attr.aria-label]="'Revoke ' + a.name" (click)="show({ kind: 'revoke-app', app: a })">Revoke</button>
                  </li>
                }
              </ul>
            }
            <div>
              <button rmBtn type="button" color="neutral" variant="subtle" data-testid="new-app-password" (click)="show({ kind: 'new-app' })"><rm-icon name="plus" class="size-4" />New app password</button>
            </div>
          </section>

          <!-- Setting up a mail program -->
          @if (mailSettings(); as m) {
            @if (m.imap.length || m.smtp.length) {
              <section aria-labelledby="h-mail-apps" class="flex flex-col gap-3" data-testid="section-mail-apps">
                <h2 id="h-mail-apps" class="text-base font-semibold text-highlighted">Set up a mail program</h2>
                <p class="text-sm text-muted">
                  Thunderbird and many other programs find these settings themselves from your address.
                  Log in with your address and {{ passwordOpensMail() && hasPassword() ? 'your password or an app password' : 'an app password' }}.
                </p>
                <dl class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 rounded-lg border border-default p-3 text-sm" data-testid="mail-settings">
                  <dt class="text-muted">User name</dt>
                  <dd class="select-all break-all font-mono text-highlighted">{{ m.username }}</dd>
                  @for (x of m.imap; track x.port) {
                    <dt class="text-muted">Incoming (IMAP)</dt>
                    <dd class="break-all font-mono text-highlighted" data-testid="mail-settings-imap">{{ x.host }}, port {{ x.port }}, {{ x.security }}</dd>
                  }
                  @for (x of m.smtp; track x.port) {
                    <dt class="text-muted">Outgoing (SMTP)</dt>
                    <dd class="break-all font-mono text-highlighted" data-testid="mail-settings-smtp">{{ x.host }}, port {{ x.port }}, {{ x.security }}</dd>
                  }
                  <dt class="text-muted">Calendars and contacts</dt>
                  <dd class="break-all font-mono text-highlighted" data-testid="mail-settings-dav">{{ davURL }}</dd>
                </dl>
                <p class="text-sm text-muted">Calendar and contact programs (iPhone, macOS, Thunderbird, DAVx⁵) take that address as CalDAV and CardDAV server, or find it from your address.</p>
                @if (m.profile) {
                  <div class="flex flex-col gap-2">
                    <div>
                      <a rmBtn color="neutral" variant="subtle" href="/auth/mobileconfig" download="mail.mobileconfig" data-testid="mobileconfig"><rm-icon name="download" class="size-4" />Profile for iPhone, iPad and Mac</a>
                    </div>
                    <p class="text-sm text-muted">The profile adds this account to the Mail app, and its calendars and contacts to Calendar and Contacts. It is not signed, so the device shows it as “Unsigned”, and it holds no password: the device asks for one.</p>
                  </div>
                }
              </section>
            }
          }

          <!-- Two-step login -->
          <section aria-labelledby="h-totp" class="flex flex-col gap-3" data-testid="section-totp">
            <h2 id="h-totp" class="text-base font-semibold text-highlighted">Two-step login</h2>
            @if (!totp().enabled) {
              <p class="text-sm text-muted">
                With two-step login the webmail asks for your password and then for a code from an authenticator app on your phone.
                Somebody who learns your password cannot get in without the phone.
              </p>
              @if (viaProvider()) {
                <p class="text-sm text-muted">When you log in through your provider, the provider decides about a second step, not robinmail.</p>
              }
              <div><button rmBtn type="button" data-testid="totp-on" (click)="show({ kind: 'totp-password' })"><rm-icon name="smartphone" class="size-4" />Turn on two-step login</button></div>
            } @else {
              <div class="${NOTE} bg-success/10 ring-success/25" data-testid="totp-enabled">
                <rm-icon name="shield-check" class="mt-0.5 size-4 text-success" />
                <div><p class="font-medium text-success">Two-step login is on.</p><p class="text-success">{{ totp().recoveryCodes }} of 10 recovery codes are left.</p></div>
              </div>
              <div class="flex flex-wrap gap-2">
                <button rmBtn type="button" color="neutral" variant="subtle" data-testid="new-codes" (click)="show({ kind: 'new-codes' })">New recovery codes</button>
                <button rmBtn type="button" color="error" variant="ghost" data-testid="totp-off" (click)="show({ kind: 'totp-off' })">Turn off</button>
              </div>
            }
          </section>

          <!-- Sessions -->
          <section aria-labelledby="h-sessions" class="flex flex-col gap-3" data-testid="section-sessions">
            <h2 id="h-sessions" class="text-base font-semibold text-highlighted">Where you are logged in</h2>
            <p class="text-sm text-muted">Browsers that are logged in to the webmail. A browser is logged out by itself when it was not used for a while.</p>
            <ul class="flex flex-col gap-2" aria-label="Sessions">
              @for (s of sessions(); track s.id) {
                <li class="flex items-start gap-3 rounded-lg border border-default p-3" data-testid="session">
                  <rm-icon [name]="phone(s.device) ? 'smartphone' : 'monitor'" class="mt-0.5 size-5 text-dimmed" />
                  <div class="min-w-0 flex-1 text-sm">
                    <p class="flex items-center gap-2 font-medium text-highlighted">
                      <span class="truncate" [title]="s.agent">{{ s.device }}</span>
                      @if (s.current) {
                        <span rmBadge>This browser</span>
                      }
                    </p>
                    <p class="text-muted" [title]="fullDate(s.createdAt)">Logged in {{ shortDate(s.createdAt) }} {{ methodText(s.method) }}, from {{ s.ip }}</p>
                    <p class="text-muted" [title]="fullDate(s.seenAt)">Last seen {{ shortDate(s.seenAt) }}@if (s.seenIp !== s.ip) { from {{ s.seenIp }}}</p>
                  </div>
                  <button
                    rmBtn type="button" size="sm" color="neutral" variant="ghost" [attr.aria-label]="'Log out ' + s.device + (s.current ? ' (this browser)' : '')"
                    (click)="show({ kind: 'revoke-session', session: s })"
                  >Log out</button>
                </li>
              }
            </ul>
            @if (sessions().length > 1) {
              <div><button rmBtn type="button" color="neutral" variant="subtle" data-testid="revoke-others" (click)="show({ kind: 'revoke-others' })">Log out everywhere else</button></div>
            }
          </section>
        </div>
      }
    </rm-settings-shell>

    @let d = dialog();
    <rm-modal [open]="d !== null" (openChange)="$event || close()" [title]="d ? titles[d.kind] : ''" [dismissible]="d?.kind !== 'app-secret' && d?.kind !== 'codes'">
      <form id="security-form" class="flex flex-col gap-4" (ngSubmit)="submit()">
        @if (error()) {
          <div class="${ALERT}" role="alert"><rm-icon name="circle-alert" class="mt-0.5 size-4" /><span>{{ error() }}</span></div>
        }

        @if (d?.kind === 'new-app') {
          <div class="flex flex-col gap-1">
            <label class="${LABEL}"><span>Name</span><input rmInput name="name" maxlength="100" required autofocus autocomplete="off" [(ngModel)]="fName"></label>
            <p class="text-sm text-muted">What it is for, so you know which one to revoke later: “Phone”, “Thunderbird at work”.</p>
          </div>
          <fieldset class="flex flex-col gap-2">
            <legend class="mb-1 text-sm font-medium text-default">It may be used for</legend>
            <label class="flex items-center gap-2 text-sm"><input type="checkbox" name="imap" [(ngModel)]="fImap">Reading mail (IMAP)</label>
            <label class="flex items-center gap-2 text-sm"><input type="checkbox" name="smtp" [(ngModel)]="fSmtp">Sending mail (SMTP)</label>
            <label class="flex items-center gap-2 text-sm"><input type="checkbox" name="jmap" [(ngModel)]="fJmap">JMAP programs</label>
            <label class="flex items-center gap-2 text-sm"><input type="checkbox" name="dav" [(ngModel)]="fDav">Calendars and contacts (CalDAV, CardDAV)</label>
          </fieldset>
          @if (hasPassword()) {
            <div class="flex flex-col gap-1">
              <label class="${LABEL}"><span>Your account password</span><input rmInput type="password" name="password" autocomplete="current-password" required [(ngModel)]="fPassword"></label>
              <p class="text-sm text-muted">To make sure it is you.</p>
            </div>
          }
        } @else if (d?.kind === 'app-secret') {
          <p class="text-sm">
            This is the password for <span class="font-medium">{{ d.app.name }}</span>. Put it into the mail program now:
            it is shown this once and cannot be looked up later.
          </p>
          <div class="flex items-center gap-2 rounded-lg bg-elevated p-3">
            <code class="min-w-0 flex-1 select-all break-all font-mono text-sm text-highlighted" data-testid="app-secret">{{ d.app.secret }}</code>
            <button rmBtn type="button" color="neutral" variant="ghost" aria-label="Copy the app password" (click)="copy(d.app.secret ?? '')"><rm-icon name="copy" class="size-4" /></button>
          </div>
          <dl class="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-sm">
            <dt class="text-dimmed">User name</dt>
            <dd class="break-all">{{ session()?.username }}</dd>
            <dt class="text-dimmed">Incoming (IMAP)</dt>
            <dd>{{ host }}, port 993, TLS</dd>
            <dt class="text-dimmed">Outgoing (SMTP)</dt>
            <dd>{{ host }}, port 465, TLS</dd>
          </dl>
          <p class="text-sm text-muted">The dashes are there for reading; with or without them it is the same password.</p>
        } @else if (d?.kind === 'revoke-app') {
          <p class="text-sm">
            The program that uses <span class="font-medium">{{ d.app.name }}</span> is logged out at once and cannot log in again with it.
            Your other app passwords keep working.
          </p>
        } @else if (d?.kind === 'totp-password') {
          <p class="text-sm">You need an authenticator app on your phone, such as Aegis, 2FAS, FreeOTP or the one of your password manager.</p>
          <p class="text-sm text-muted">Once it is on, your account password no longer works in mail programs: they need an app password.</p>
          @if (hasPassword()) {
            <div class="flex flex-col gap-1">
              <label class="${LABEL}"><span>Your account password</span><input rmInput type="password" name="password" autocomplete="current-password" required autofocus [(ngModel)]="fPassword"></label>
              <p class="text-sm text-muted">To make sure it is you.</p>
            </div>
          }
        } @else if (d?.kind === 'totp-scan') {
          <p class="text-sm">Scan this with the authenticator app, then type the six digits it shows.</p>
          <!-- Dark on white whatever the theme: that is what cameras read. -->
          @if (d.enrolment.qr?.length) {
            <div class="self-center rounded-lg bg-white p-2">
              <svg
                [attr.viewBox]="'-4 -4 ' + (d.enrolment.qr.length + 8) + ' ' + (d.enrolment.qr.length + 8)" class="size-52" role="img"
                aria-label="QR code with the secret for the authenticator app" shape-rendering="crispEdges" data-testid="totp-qr"
              >
                <path [attr.d]="qrPath(d.enrolment.qr)" fill="#000" />
              </svg>
            </div>
          }
          <details class="text-sm">
            <summary class="cursor-pointer text-muted">Cannot scan it? Type this key into the app instead</summary>
            <div class="mt-2 flex items-center gap-2 rounded-lg bg-elevated p-3">
              <code class="min-w-0 flex-1 select-all break-all font-mono text-sm" data-testid="totp-secret">{{ d.enrolment.secret }}</code>
              <button rmBtn type="button" color="neutral" variant="ghost" aria-label="Copy the key" (click)="copy(d.enrolment.secret)"><rm-icon name="copy" class="size-4" /></button>
            </div>
            <p class="mt-1 text-muted">Time-based, 6 digits, 30 seconds, SHA-1: what an app uses unless told otherwise.</p>
          </details>
          <label class="${LABEL}">
            <span>Code</span>
            <input rmInput class="tracking-widest" name="code" inputmode="numeric" autocomplete="one-time-code" maxlength="9" required autofocus placeholder="123456" [(ngModel)]="fCode">
          </label>
        } @else if (d?.kind === 'codes') {
          <p class="text-sm">
            @if (d.fresh) {Two-step login is on. }Keep these recovery codes somewhere safe, not on the phone. Each one logs you in once when you do not have the phone.
            They are shown this once@if (!d.fresh) {; the old ones no longer work}.
          </p>
          <ul class="grid grid-cols-2 gap-x-4 gap-y-1 rounded-lg bg-elevated p-3 font-mono text-sm" data-testid="recovery-codes">
            @for (c of d.codes; track c) {
              <li class="select-all">{{ c }}</li>
            }
          </ul>
          <div><button rmBtn type="button" color="neutral" variant="subtle" size="sm" (click)="copy(d.codes.join('\\n'))"><rm-icon name="copy" class="size-4" />Copy all</button></div>
        } @else if (d?.kind === 'totp-off' || d?.kind === 'new-codes') {
          <p class="text-sm">
            {{ d.kind === 'totp-off'
              ? 'After this the webmail asks for your password only. The authenticator app and the recovery codes stop working.'
              : 'You get ten new recovery codes. The ones you have stop working.' }}
          </p>
          @if (hasPassword()) {
            <label class="${LABEL}"><span>Your account password</span><input rmInput type="password" name="password" autocomplete="current-password" required autofocus [(ngModel)]="fPassword"></label>
          }
          <div class="flex flex-col gap-1">
            <label class="${LABEL}"><span>Code</span><input rmInput name="code" autocomplete="one-time-code" autocapitalize="off" spellcheck="false" maxlength="40" required [(ngModel)]="fCode"></label>
            <p class="text-sm text-muted">From the authenticator app, or a recovery code.</p>
          </div>
        } @else if (d?.kind === 'revoke-session') {
          <p class="text-sm">
            @if (d.session.current) {
              This is the browser you are using: you will be logged out here.
            } @else {
              {{ d.session.device }}, last seen {{ shortDate(d.session.seenAt) }} from {{ d.session.seenIp }}, will have to log in again.
            }
          </p>
        } @else if (d?.kind === 'revoke-others') {
          <p class="text-sm">Every browser but this one will have to log in again. Mail programs with an app password are not affected.</p>
        }
      </form>
      <ng-container modal-footer>
        @if (d?.kind === 'app-secret' || d?.kind === 'codes') {
          <button rmBtn type="button" data-testid="dialog-done" (click)="close()">I have saved it</button>
        } @else {
          <button rmBtn type="button" variant="ghost" color="neutral" (click)="close()">Cancel</button>
          <button rmBtn type="submit" form="security-form" [disabled]="busy()" data-testid="dialog-submit" [color]="d?.kind === 'revoke-app' || d?.kind === 'totp-off' ? 'error' : 'primary'">
            {{ submitLabel() }}
          </button>
        }
      </ng-container>
    </rm-modal>
  `,
})
export class SecurityPage implements OnInit {
  private readonly router = inject(Router)
  private readonly toaster = inject(Toaster)
  protected readonly titles = titles
  protected readonly fullDate = fullDate
  protected readonly shortDate = shortDate
  protected readonly qrPath = api.qrPath
  protected readonly host = window.location.hostname

  protected readonly session = signal<api.Session | null>(null)
  protected readonly appPasswords = signal<api.AppPassword[]>([])
  protected readonly passwordOpensMail = signal(true)
  protected readonly totp = signal({ enabled: false, recoveryCodes: 0 })
  protected readonly sessions = signal<api.WebSession[]>([])
  protected readonly mailSettings = signal<api.MailSettings | null>(null)
  protected readonly loaded = signal(false)
  protected readonly loadError = signal('')

  protected readonly hasPassword = computed(() => this.session()?.hasPassword ?? true)
  protected readonly viaProvider = computed(() => this.session()?.method.startsWith('oidc:') ?? false)

  protected readonly dialog = signal<Dialog | null>(null)
  protected readonly busy = signal(false)
  protected readonly error = signal('')
  protected readonly fName = signal('')
  protected readonly fImap = signal(true)
  protected readonly fSmtp = signal(true)
  protected readonly fJmap = signal(true)
  protected readonly fDav = signal(true)
  protected readonly davURL = location.origin + '/dav/'
  protected readonly fPassword = signal('')
  protected readonly fCode = signal('')
  protected readonly submitLabel = computed(() => submitLabels[this.dialog()?.kind ?? 'revoke-others'] ?? 'Log out')

  // The password form is on the page itself.
  protected readonly pwCurrent = signal('')
  protected readonly pwNext = signal('')
  protected readonly pwAgain = signal('')
  protected readonly pwBusy = signal(false)
  protected readonly pwError = signal('')

  ngOnInit(): void {
    document.title = 'Security · robinmail'
    void this.load()
  }

  // gone handles an answer that says the login ended: back to the login page.
  private gone(e: unknown): boolean {
    if (!(e instanceof api.AuthError) || e.status !== 401 || e.message === 'wrong code') return false
    auth.account = null
    compose.clear()
    store.reset()
    this.dialog.set(null)
    void this.router.navigate(['/login'], { queryParams: { next: '/settings/security' }, replaceUrl: true })
    return true
  }

  protected async load(): Promise<void> {
    this.loadError.set('')
    try {
      const [who, apps, t, s] = await Promise.all([api.currentSession(), api.appPasswords(), api.totpState(), api.sessions()])
      if (!who.session) throw new api.AuthError(401, 'not logged in')
      this.session.set(who.session)
      this.appPasswords.set(apps.list)
      this.passwordOpensMail.set(apps.passwordOpensMail)
      this.totp.set(t)
      this.sessions.set(s)
      this.loaded.set(true)
      // What a mail program needs. The page works without it.
      this.mailSettings.set(await api.mailSettings().catch(() => null))
    } catch (e) {
      if (!this.gone(e)) this.loadError.set(api.explain(e))
    }
  }

  protected show(d: Dialog): void {
    this.dialog.set(d)
    this.error.set('')
    this.busy.set(false)
    this.fName.set('')
    this.fImap.set(true)
    this.fSmtp.set(true)
    this.fJmap.set(true)
    this.fDav.set(true)
    this.fPassword.set('')
    this.fCode.set('')
  }
  protected close(): void {
    this.dialog.set(null)
    this.fPassword.set('')
    this.fCode.set('')
  }

  // run does what a dialog's form asks and shows what went wrong in it.
  private async run(work: () => Promise<void>): Promise<void> {
    if (this.busy()) return
    this.busy.set(true)
    this.error.set('')
    try {
      await work()
    } catch (e) {
      if (!this.gone(e)) this.error.set(api.explain(e))
    } finally {
      this.busy.set(false)
    }
  }

  protected submit(): Promise<void> | void {
    const d = this.dialog()
    if (!d) return
    switch (d.kind) {
      case 'new-app':
        return this.run(async () => {
          const all = this.fImap() && this.fSmtp() && this.fJmap() && this.fDav()
          const scopes = all ? [] : [this.fImap() && 'imap', this.fSmtp() && 'smtp', this.fJmap() && 'jmap', this.fDav() && 'dav'].filter((s): s is string => !!s)
          if (!all && scopes.length === 0) throw new api.AuthError(400, 'choose at least one kind of program')
          const app = await api.createAppPassword(this.fName().trim(), scopes, this.fPassword())
          await this.load()
          this.show({ kind: 'app-secret', app })
        })
      case 'revoke-app':
        return this.run(async () => {
          await api.revokeAppPassword(d.app.id)
          await this.load()
          this.close()
          this.toaster.add({ title: `Revoked “${d.app.name}”`, description: 'Mail programs that used it are logged out.', icon: 'check' })
        })
      case 'totp-password':
        return this.run(async () => {
          const enrolment = await api.totpEnrol(this.fPassword())
          this.show({ kind: 'totp-scan', enrolment })
        })
      case 'totp-scan':
        return this.run(async () => {
          const codes = await api.totpConfirm(this.fCode().replace(/\s/g, ''))
          await this.load()
          this.show({ kind: 'codes', codes, fresh: true })
        })
      case 'totp-off':
        return this.run(async () => {
          await api.totpDisable(this.fPassword(), this.fCode())
          await this.load()
          this.close()
          this.toaster.add({ title: 'Two-step login is off', icon: 'check' })
        })
      case 'new-codes':
        return this.run(async () => {
          const codes = await api.newRecoveryCodes(this.fPassword(), this.fCode())
          await this.load()
          this.show({ kind: 'codes', codes, fresh: false })
        })
      case 'revoke-session':
        return this.run(async () => {
          await api.revokeSession(d.session.id)
          if (d.session.current) throw new api.AuthError(401, 'logged out')
          await this.load()
          this.close()
        })
      case 'revoke-others':
        return this.run(async () => {
          const n = await api.revokeOtherSessions()
          await this.load()
          this.close()
          this.toaster.add({ title: n === 1 ? 'Logged out 1 other browser' : `Logged out ${n} other browsers`, icon: 'check' })
        })
      default:
        this.close()
    }
  }

  protected async changePassword(): Promise<void> {
    if (this.pwBusy()) return
    this.pwError.set('')
    if (this.pwNext() !== this.pwAgain()) {
      this.pwError.set('The two new passwords are not the same.')
      return
    }
    this.pwBusy.set(true)
    try {
      await api.changePassword(this.pwCurrent(), this.pwNext())
      this.pwCurrent.set('')
      this.pwNext.set('')
      this.pwAgain.set('')
      await this.load()
      this.toaster.add({ title: 'Password changed', description: 'Every other browser was logged out.', icon: 'check' })
    } catch (e) {
      if (!this.gone(e)) this.pwError.set(api.explain(e))
    } finally {
      this.pwBusy.set(false)
    }
  }

  protected async copy(text: string): Promise<void> {
    try {
      await navigator.clipboard.writeText(text)
      this.toaster.add({ title: 'Copied', icon: 'clipboard-check', duration: 1500 })
    } catch {
      this.toaster.add({ title: 'Could not copy: select the text and copy it yourself' })
    }
  }

  protected phone(device: string): boolean {
    return /iPhone|Android/.test(device)
  }
  protected scopeText(a: api.AppPassword): string {
    return a.scopes.length ? a.scopes.map((s) => scopeNames[s] ?? s).join(', ') : 'reading, sending and JMAP'
  }
  protected methodText(m: string): string {
    const [first, second] = m.split('+')
    const how = first!.startsWith('oidc:') ? `through ${first!.slice(5)}` : 'with the password'
    return second === 'totp' ? `${how} and a code` : second === 'recovery' ? `${how} and a recovery code` : how
  }
}
