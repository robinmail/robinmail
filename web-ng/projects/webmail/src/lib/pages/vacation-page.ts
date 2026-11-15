// Vacation: the automatic reply of the account (JMAP VacationResponse,
// RFC 8621 8). The server sends it at most once a week to each sender, and
// never to lists, notifications or other automatic mail.

import { ChangeDetectionStrategy, Component, type OnInit, computed, inject, signal } from '@angular/core'
import { FormsModule } from '@angular/forms'
import { MethodError } from '@robinmail/jmap'
import { auth, client, loadAccount } from '../app'
import { SettingsShell } from '../components/settings-shell'
import { ALERT, Btn, Input, LABEL, SKELETON } from '../ui/controls'
import { Icon } from '../ui/icon'
import { Switch } from '../ui/switch'
import { Toaster } from '../ui/toast'
import { fromLocalInput, toLocalInput } from '../util/local-date'

const blank = () => ({ enabled: false, from: '', to: '', subject: '', text: '' })

@Component({
  selector: 'rm-vacation-page',
  imports: [FormsModule, SettingsShell, Btn, Input, Icon, Switch],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'block h-full overflow-y-auto' },
  template: `
    <rm-settings-shell title="Vacation">
      @if (loadError()) {
        <div class="flex flex-col items-center gap-3 pt-16 text-center">
          <rm-icon name="cloud-off" class="size-10 text-dimmed" />
          <p class="text-sm text-muted">The automatic reply could not be loaded.<br>{{ loadError() }}</p>
          <button rmBtn type="button" (click)="load()">Try again</button>
        </div>
      } @else if (loading()) {
        <div class="flex flex-col gap-3" aria-busy="true">
          @for (n of [1, 2, 3]; track n) {
            <div class="h-12 w-full ${SKELETON}"></div>
          }
        </div>
      } @else {
        <form class="flex flex-col gap-4" (ngSubmit)="save()">
          <p class="text-sm text-muted">
            While it is on, people who write to you get this reply: once a week each, and never mailing lists,
            newsletters or other automatic mail. Their message arrives as usual.
          </p>
          @if (error()) {
            <div class="${ALERT}" role="alert"><rm-icon name="circle-alert" class="mt-0.5 size-4" /><span>{{ error() }}</span></div>
          }
          <rm-switch [checked]="enabled()" (checkedChange)="enabled.set($event)" label="Send an automatic reply" testid="vacation-enabled" />
          <div class="grid gap-4 sm:grid-cols-2">
            <div class="flex flex-col gap-1">
              <label class="${LABEL}"><span>From</span><input rmInput type="datetime-local" name="from" [(ngModel)]="from"></label>
              <p class="text-sm text-muted">Empty: from now on.</p>
            </div>
            <div class="flex flex-col gap-1">
              <label class="${LABEL}"><span>Until</span><input rmInput type="datetime-local" name="to" [(ngModel)]="to"></label>
              <p class="text-sm text-muted">Empty: until you turn it off.</p>
            </div>
          </div>
          <div class="flex flex-col gap-1">
            <label class="${LABEL}"><span>Subject</span><input rmInput name="subject" maxlength="200" autocomplete="off" [(ngModel)]="subject"></label>
            <p class="text-sm text-muted">Empty: “Auto:” and the subject of the message you got.</p>
          </div>
          <label class="${LABEL}">
            <span>Message</span>
            <textarea rmInput name="text" rows="6" placeholder="I am away until Monday and will answer when I am back." [(ngModel)]="text"></textarea>
          </label>
          @if (html() !== null && text() === saved().text) {
            <p class="text-xs text-dimmed">Another program gave this reply a formatted version too. It is kept until you change the message here.</p>
          }
          <div><button rmBtn type="submit" [disabled]="busy() || !changed()" data-testid="vacation-save">Save</button></div>
        </form>
      }
    </rm-settings-shell>
  `,
})
export class VacationPage implements OnInit {
  private readonly toaster = inject(Toaster)
  protected readonly loadError = signal('')
  protected readonly loading = signal(true)
  protected readonly busy = signal(false)
  protected readonly error = signal('')

  protected readonly enabled = signal(false)
  protected readonly from = signal('')
  protected readonly to = signal('')
  protected readonly subject = signal('')
  protected readonly text = signal('')
  private readonly form = computed(() => ({ enabled: this.enabled(), from: this.from(), to: this.to(), subject: this.subject(), text: this.text() }))
  protected readonly saved = signal(blank())
  // An HTML body another client set stays as it is unless the text changes.
  protected readonly html = signal<string | null>(null)
  protected readonly changed = computed(() => JSON.stringify(this.form()) !== JSON.stringify(this.saved()))

  ngOnInit(): void {
    document.title = 'Vacation · robinmail'
    void this.load()
  }

  protected async load(): Promise<void> {
    this.loadError.set('')
    this.loading.set(true)
    try {
      await loadAccount()
      const b = client.batch()
      const get = b.call('VacationResponse/get', { ids: ['singleton'] })
      const v = (await b.send()).get(get).list[0]
      if (v) {
        this.enabled.set(v.isEnabled)
        this.from.set(toLocalInput(v.fromDate))
        this.to.set(toLocalInput(v.toDate))
        this.subject.set(v.subject ?? '')
        this.text.set(v.textBody ?? '')
        this.html.set(v.htmlBody)
      }
      this.saved.set({ ...this.form() })
    } catch (e) {
      if (auth.account) this.loadError.set(e instanceof Error ? e.message : String(e))
    } finally {
      this.loading.set(false)
    }
  }

  protected async save(): Promise<void> {
    if (this.busy()) return
    this.error.set('')
    const f = this.form()
    const from = fromLocalInput(f.from)
    const to = fromLocalInput(f.to)
    if (from && to && from >= to) {
      this.error.set('The last day must be after the first.')
      return
    }
    this.busy.set(true)
    try {
      const b = client.batch()
      const set = b.call('VacationResponse/set', {
        update: {
          singleton: {
            isEnabled: f.enabled,
            fromDate: from,
            toDate: to,
            subject: f.subject.trim() || null,
            textBody: f.text.trim() ? f.text : null,
            // A text typed here replaces an HTML body from elsewhere: the
            // two would otherwise say different things.
            ...(f.text !== this.saved().text ? { htmlBody: null } : {}),
          },
        },
      })
      const r = (await b.send()).get(set)
      const failed = r.notUpdated?.['singleton']
      if (failed) {
        this.error.set(failed.description || failed.type)
        return
      }
      await this.load()
      this.toaster.add({ title: f.enabled ? 'The automatic reply is on' : 'The automatic reply is off', icon: 'check' })
    } catch (e) {
      this.error.set(e instanceof MethodError ? e.description || e.type : e instanceof Error ? e.message : String(e))
    } finally {
      this.busy.set(false)
    }
  }
}
