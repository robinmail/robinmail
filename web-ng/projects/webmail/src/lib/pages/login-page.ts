// The login: address and password, then, for an account with two-step
// login, a code. Or a provider, when the server has any configured.

import { ChangeDetectionStrategy, Component, type OnInit, inject, signal } from '@angular/core'
import { FormsModule } from '@angular/forms'
import { ActivatedRoute, Router } from '@angular/router'
import { authApi } from '@robinmail/jmap'
import { auth } from '../app'
import { safeNext } from '../session'
import { ColorModeButton } from '../ui/color-mode'
import { ALERT, Btn, CARD, Input, LABEL } from '../ui/controls'
import { Icon } from '../ui/icon'

// What a login through a provider came back with (see oidc.go).
const providerErrors: Record<string, string> = {
  oidc_denied: 'The login at the provider was cancelled.',
  oidc_no_account: 'There is no mailbox here for the account you logged in with at the provider.',
  oidc_unverified: 'The provider has not verified your email address, so it cannot be used to log in here.',
  oidc_unavailable: 'The provider cannot be reached right now. Try again, or log in with your password.',
  oidc_failed: 'The login through the provider did not work. Try again.',
  too_many: 'Too many failed logins. Try again in a few minutes.',
}

@Component({
  selector: 'rm-login-page',
  imports: [FormsModule, Btn, Input, Icon, ColorModeButton],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'block h-full' },
  template: `
    <main class="flex min-h-full flex-col items-center justify-center gap-6 bg-muted p-4">
      <div class="flex items-center gap-2 text-highlighted">
        <img src="/favicon.svg" alt="" class="size-8">
        <span class="text-2xl font-semibold tracking-tight">robinmail</span>
      </div>
      <div class="w-full max-w-sm p-6 ${CARD}">
        @if (step() === 'password') {
          <form class="flex flex-col gap-4" data-testid="login-form" (ngSubmit)="submit()">
            <h1 class="text-lg font-semibold text-highlighted">Log in</h1>
            @if (error()) {
              <div class="${ALERT}" role="alert"><rm-icon name="circle-alert" class="mt-0.5 size-4" /><span>{{ error() }}</span></div>
            }
            <label class="${LABEL}">
              <span>Email address</span>
              <input rmInput class="!py-2" type="email" name="username" autocomplete="username" required autofocus placeholder="you@example.com" [(ngModel)]="username">
            </label>
            <label class="${LABEL}">
              <span>Password</span>
              <input rmInput class="!py-2" type="password" name="password" autocomplete="current-password" required [(ngModel)]="password">
            </label>
            <div class="flex items-start gap-2 text-sm">
              <input type="checkbox" id="remember" name="remember" class="mt-0.5" aria-describedby="remember-help" [(ngModel)]="remember">
              <div>
                <label for="remember" class="font-medium text-default">Remember this device</label>
                <p id="remember-help" class="text-muted">Stay logged in for up to 30 days. Not on a shared computer.</p>
              </div>
            </div>
            <button rmBtn type="submit" size="lg" [block]="true" [disabled]="busy()">
              @if (busy()) {
                <rm-icon name="loader-circle" class="size-4 animate-spin" />
              }
              Log in
            </button>
            @if (providers().length) {
              <div class="flex items-center gap-3 text-xs text-muted" role="separator">
                <span class="h-px flex-1 bg-accented"></span>or<span class="h-px flex-1 bg-accented"></span>
              </div>
              @for (p of providers(); track p.name) {
                <a rmBtn variant="subtle" color="neutral" size="lg" [block]="true" [href]="providerLink(p)" [attr.data-testid]="'provider-' + p.name">
                  <rm-icon name="key-round" class="size-4" />Log in with {{ p.label }}
                </a>
              }
            }
          </form>
        } @else {
          <form class="flex flex-col gap-4" data-testid="code-form" (ngSubmit)="submitCode()">
            <h1 class="text-lg font-semibold text-highlighted">Two-step login</h1>
            @if (error()) {
              <div class="${ALERT}" role="alert"><rm-icon name="circle-alert" class="mt-0.5 size-4" /><span>{{ error() }}</span></div>
            }
            @if (!useRecovery()) {
              <div class="flex flex-col gap-1">
                <label class="${LABEL}">
                  <span>Code</span>
                  <input
                    rmInput class="!py-2 tracking-widest" name="code" inputmode="numeric" autocomplete="one-time-code" pattern="[0-9 ]*" maxlength="9" required autofocus
                    placeholder="123456" [(ngModel)]="code"
                  >
                </label>
                <p class="text-sm text-muted">The six digits your authenticator app shows for robinmail.</p>
              </div>
            } @else {
              <div class="flex flex-col gap-1">
                <label class="${LABEL}">
                  <span>Recovery code</span>
                  <input
                    rmInput class="!py-2" name="code" autocomplete="off" autocapitalize="off" spellcheck="false" maxlength="40" required autofocus
                    placeholder="abcd-efgh-ijkl-mnop" [(ngModel)]="code"
                  >
                </label>
                <p class="text-sm text-muted">One of the codes you saved when you turned two-step login on. Each works once.</p>
              </div>
            }
            <button rmBtn type="submit" size="lg" [block]="true" [disabled]="busy()">Log in</button>
            <div class="flex items-center justify-between gap-2 text-sm">
              <button rmBtn type="button" variant="link" color="neutral" data-testid="toggle-recovery" (click)="toggleRecovery()">
                {{ useRecovery() ? 'Use a code from the app' : 'Use a recovery code' }}
              </button>
              <button rmBtn type="button" variant="link" color="neutral" (click)="startOver()">Start over</button>
            </div>
          </form>
        }
      </div>
      <rm-color-mode-button />
    </main>
  `,
})
export class LoginPage implements OnInit {
  private readonly route = inject(ActivatedRoute)
  private readonly router = inject(Router)

  protected readonly username = signal('')
  protected readonly password = signal('')
  protected readonly remember = signal(false)
  protected readonly code = signal('')
  protected readonly useRecovery = signal(false)
  protected readonly busy = signal(false)
  protected readonly error = signal('')
  protected readonly providers = signal<authApi.Provider[]>([])
  // 'totp' once the password was right and the account wants a code.
  protected readonly step = signal<'password' | 'totp'>(auth.waiting || this.route.snapshot.queryParamMap.get('step') === 'totp' ? 'totp' : 'password')

  async ngOnInit(): Promise<void> {
    document.title = 'Log in · robinmail'
    const e = this.route.snapshot.queryParamMap.get('error')
    if (e) this.error.set(providerErrors[e] ?? providerErrors['oidc_failed']!)
    try {
      this.providers.set(await authApi.providers())
    } catch {
      // Without the list there are no buttons; the password still works.
    }
  }

  protected providerLink(p: authApi.Provider): string {
    return `/auth/oidc/${encodeURIComponent(p.name)}/start${this.remember() ? '?remember=1' : ''}`
  }

  private async done(): Promise<void> {
    this.password.set('')
    this.code.set('')
    auth.waiting = false
    await this.router.navigateByUrl(safeNext(this.route.snapshot.queryParamMap.get('next')), { replaceUrl: true })
  }

  private loginFailed(e: unknown): void {
    if (e instanceof authApi.AuthError && e.status === 401) this.error.set('Wrong address or password.')
    else if (e instanceof authApi.AuthError && e.status === 429) {
      const minutes = Math.max(1, Math.ceil(e.retryAfter / 60))
      this.error.set(`Too many failed logins. Try again in ${minutes} ${minutes === 1 ? 'minute' : 'minutes'}.`)
    } else if (e instanceof authApi.AuthError) this.error.set(`The server cannot log you in right now (${e.status}).`)
    else this.error.set('The server does not answer. Check the connection and try again.')
  }

  protected async submit(): Promise<void> {
    if (this.busy()) return
    this.busy.set(true)
    this.error.set('')
    try {
      const answer = await authApi.login(this.username().trim(), this.password(), this.remember())
      this.password.set('')
      if ('step' in answer) {
        this.step.set('totp')
        setTimeout(() => document.querySelector<HTMLInputElement>('input[name="code"]')?.focus())
      } else {
        auth.account = answer
        await this.done()
      }
    } catch (e) {
      this.loginFailed(e)
    } finally {
      this.busy.set(false)
    }
  }

  protected async submitCode(): Promise<void> {
    if (this.busy()) return
    this.busy.set(true)
    this.error.set('')
    try {
      auth.account = await authApi.loginCode(this.useRecovery() ? { recoveryCode: this.code().trim() } : authApi.secondProof(this.code()))
      await this.done()
    } catch (e) {
      this.code.set('')
      if (e instanceof authApi.AuthError && e.status === 401 && e.message !== 'wrong code') {
        // The login that waited is gone: five minutes passed, or it was used.
        this.step.set('password')
        this.error.set('That took too long. Log in again.')
      } else if (e instanceof authApi.AuthError && e.status === 401) {
        this.error.set(this.useRecovery() ? 'That recovery code is not right, or was used before.' : 'That code is not right. Codes change every 30 seconds and work once.')
      } else this.error.set(authApi.explain(e))
    } finally {
      this.busy.set(false)
    }
  }

  protected toggleRecovery(): void {
    this.useRecovery.update((v) => !v)
    this.code.set('')
    this.error.set('')
  }

  protected async startOver(): Promise<void> {
    this.error.set('')
    this.code.set('')
    this.useRecovery.set(false)
    this.step.set('password')
    auth.waiting = false
    try {
      await authApi.logout() // forgets the login that waits
    } catch {
      // it ends by itself in five minutes
    }
  }
}
