// Filters: what happens to mail when it arrives. The server runs one Sieve
// script per account (RFC 5228, managed over JMAP, RFC 9661). This page
// shows that script as a list of simple rules when it is one the page
// wrote itself (sieve/rules.ts), and as text otherwise; the text can be
// edited either way.

import { ChangeDetectionStrategy, Component, type OnInit, computed, inject, signal } from '@angular/core'
import { FormsModule } from '@angular/forms'
import { MethodError, type SetError, type SieveScript, sieveRules } from '@robinmail/jmap'
import { auth, client, loadAccount, store } from '../app'
import { SettingsShell } from '../components/settings-shell'
import { ALERT, Btn, Input, LABEL, SKELETON } from '../ui/controls'
import { Icon } from '../ui/icon'
import { Modal } from '../ui/modal'
import { Select } from '../ui/select'
import { Switch } from '../ui/switch'
import { Toaster } from '../ui/toast'

type Rule = sieveRules.Rule
type Field = sieveRules.Field

// The name of the script the page makes when the account has none.
const NAME = 'Filters'

const fields: { label: string; value: Field }[] = [
  { label: 'From', value: 'from' },
  { label: 'To or Cc', value: 'to' },
  { label: 'Subject', value: 'subject' },
]

interface Form {
  name: string
  match: 'all' | 'any'
  conditions: { field: Field; text: string }[]
  move: boolean
  moveTo: string
  label: boolean
  labelWith: string
  read: boolean
  forward: boolean
  forwardTo: string
  remove: boolean
}

function blank(): Form {
  return { name: '', match: 'all', conditions: [{ field: 'from', text: '' }], move: false, moveTo: '', label: false, labelWith: '', read: false, forward: false, forwardTo: '', remove: false }
}

function explain(e: SetError): string {
  switch (e.type) {
    case 'invalidSieve':
      return `The script is not valid: ${e.description ?? 'the server did not say why'}`
    case 'tooLarge':
      return 'The script is too large.'
    case 'overQuota':
      return 'The account has too many scripts.'
    default:
      return e.description || e.type
  }
}

const TAB = 'inline-flex items-center gap-1.5 rounded-md px-2.5 py-1.5 text-xs font-medium text-default hover:bg-elevated focus-visible:outline-2 focus-visible:outline-primary aria-selected:bg-elevated'
const CHECK = 'flex items-center gap-2 text-sm'

@Component({
  selector: 'rm-filters-page',
  imports: [FormsModule, SettingsShell, Btn, Input, Icon, Modal, Select, Switch],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'block h-full overflow-y-auto' },
  template: `
    <rm-settings-shell title="Filters">
      @if (loadError()) {
        <div class="flex flex-col items-center gap-3 pt-16 text-center">
          <rm-icon name="cloud-off" class="size-10 text-dimmed" />
          <p class="text-sm text-muted">The filters could not be loaded.<br>{{ loadError() }}</p>
          <button rmBtn type="button" (click)="load(true)">Try again</button>
        </div>
      } @else if (loading() && scripts().length === 0) {
        <div class="flex flex-col gap-3" aria-busy="true">
          <div class="h-20 w-full ${SKELETON}"></div>
          <div class="h-20 w-full ${SKELETON}"></div>
        </div>
      } @else {
        <p class="text-sm text-muted">
          Rules look at mail as it arrives and file, mark, forward or delete it. They run in the order shown; a rule
          that moves or deletes a message is the last one that message meets.
        </p>

        <div class="flex flex-wrap items-center gap-2">
          <div class="flex gap-1" role="tablist" aria-label="How to show the filters">
            <button type="button" class="${TAB}" role="tab" [attr.aria-selected]="tab() === 'rules'" data-testid="tab-rules" (click)="tab.set('rules')"><rm-icon name="list-checks" class="size-4" />Rules</button>
            <button type="button" class="${TAB}" role="tab" [attr.aria-selected]="tab() === 'script'" data-testid="tab-script" (click)="showScript()"><rm-icon name="file-code" class="size-4" />Script</button>
          </div>
          @if (current()) {
            <rm-switch class="ml-auto" [checked]="enabled()" [disabled]="busy()" [label]="enabled() ? 'On' : 'Off'" testid="filters-enabled" (checkedChange)="setEnabled($event)" />
          }
        </div>

        @if (saveError()) {
          <div class="${ALERT}" role="alert" data-testid="filters-error"><rm-icon name="circle-alert" class="mt-0.5 size-4" /><span>{{ saveError() }}</span></div>
        }
        @if (current() && !enabled()) {
          <div class="flex items-start gap-2 rounded-md bg-warning/10 px-3 py-2 text-sm ring ring-inset ring-warning/25">
            <rm-icon name="pause" class="mt-0.5 size-4 text-warning" />
            <div><p class="font-medium text-highlighted">The filters are off</p><p class="text-muted">They are kept, but new mail goes to the Inbox as it is.</p></div>
          </div>
        }

        @if (tab() === 'rules') {
          @let list = rules();
          @if (list === null) {
            <div class="flex flex-col items-start gap-3 rounded-lg border border-default p-4" data-testid="custom-script">
              <p class="text-sm">
                This account's filter is a script that was written by hand, or by another program. It runs as it is; this
                list can only show rules the page made itself.
              </p>
              <div class="flex gap-2">
                <button rmBtn type="button" color="neutral" variant="subtle" size="sm" (click)="showScript()">Show the script</button>
                <button rmBtn type="button" color="neutral" variant="ghost" size="sm" [disabled]="busy()" (click)="startOver()">Replace it with rules</button>
              </div>
            </div>
          } @else {
            @if (list.length) {
              <ol class="flex flex-col gap-3" aria-label="Rules">
                @for (r of list; track $index; let i = $index) {
                  @let words = describe(r);
                  <li class="rounded-lg border border-default p-4" data-testid="rule">
                    <div class="flex items-start gap-1">
                      <div class="min-w-0 flex-1">
                        <p class="truncate font-medium text-highlighted">{{ r.name }}</p>
                        <p class="text-sm text-muted">If {{ words.when }}:<br><span class="text-default">{{ words.then }}</span></p>
                      </div>
                      <button rmBtn type="button" size="sm" color="neutral" variant="ghost" [disabled]="i === 0 || busy()" [attr.aria-label]="'Move ' + r.name + ' up'" (click)="moveRule(i, -1)"><rm-icon name="arrow-up" class="size-4" /></button>
                      <button rmBtn type="button" size="sm" color="neutral" variant="ghost" [disabled]="i === list.length - 1 || busy()" [attr.aria-label]="'Move ' + r.name + ' down'" (click)="moveRule(i, 1)"><rm-icon name="arrow-down" class="size-4" /></button>
                      <button rmBtn type="button" size="sm" color="neutral" variant="ghost" [attr.aria-label]="'Edit ' + r.name" (click)="edit(i)"><rm-icon name="pencil" class="size-4" /></button>
                      <button rmBtn type="button" size="sm" color="neutral" variant="ghost" [disabled]="busy()" [attr.aria-label]="'Remove ' + r.name" (click)="removeRule(i)"><rm-icon name="trash-2" class="size-4" /></button>
                    </div>
                  </li>
                }
              </ol>
            } @else {
              <p class="rounded-lg border border-dashed border-default p-6 text-center text-sm text-muted">No rules yet. All mail arrives in the Inbox.</p>
            }
            <div><button rmBtn type="button" data-testid="add-rule" [disabled]="busy()" (click)="edit(-1)"><rm-icon name="plus" class="size-4" />Add a rule</button></div>
          }
        } @else {
          <p class="text-sm text-muted">
            The filter as the server runs it: a Sieve script. What is saved here is checked by the server first. A script
            changed by hand is no longer shown as rules.
          </p>
          <textarea
            rmInput #editor rows="18" class="font-mono" spellcheck="false" autocapitalize="off" aria-label="Sieve script" data-testid="script-editor"
            [value]="draft()" (input)="draft.set(editor.value)"
          ></textarea>
          <div class="flex items-center gap-2">
            <button rmBtn type="button" [disabled]="busy() || (draft() === text() && !!current())" data-testid="save-script" (click)="saveScript()">Save</button>
            <button rmBtn type="button" color="neutral" variant="ghost" [disabled]="draft() === text()" (click)="draft.set(text())">Undo changes</button>
            @if (current(); as c) {
              <span class="ml-auto truncate text-xs text-dimmed">Script “{{ c.name }}”</span>
            }
          </div>
        }
      }
    </rm-settings-shell>

    <rm-modal [open]="editing() !== null" (openChange)="$event || editing.set(null)" [title]="editing() === -1 ? 'New rule' : 'Edit rule'" description="What to look for in a new message, and what to do with it.">
      @let f = form();
      <form id="rule-form" class="flex flex-col gap-4" (ngSubmit)="submitRule()">
        @if (formError()) {
          <div class="${ALERT}" role="alert"><rm-icon name="circle-alert" class="mt-0.5 size-4" /><span>{{ formError() }}</span></div>
        }
        <label class="${LABEL}"><span>Name</span><input rmInput name="name" maxlength="100" autofocus autocomplete="off" [(ngModel)]="f.name"></label>

        <fieldset class="flex flex-col gap-2">
          <legend class="mb-1 text-sm font-medium">If</legend>
          @for (c of f.conditions; track $index; let n = $index) {
            <div class="flex items-center gap-2" data-testid="condition">
              <rm-select class="w-32 shrink-0" [value]="c.field" [options]="fields" [label]="'Field of condition ' + (n + 1)" (valueChange)="setField(n, $event)" />
              <span class="shrink-0 text-sm text-muted">contains</span>
              <input rmInput class="min-w-0 flex-1" autocomplete="off" [attr.aria-label]="'Text of condition ' + (n + 1)" [name]="'text' + n" [(ngModel)]="c.text">
              @if (f.conditions.length > 1) {
                <button rmBtn type="button" size="sm" color="neutral" variant="ghost" [attr.aria-label]="'Remove condition ' + (n + 1)" (click)="removeCondition(n)"><rm-icon name="x" class="size-4" /></button>
              }
            </div>
          }
          <div class="flex flex-wrap items-center gap-3">
            <button rmBtn type="button" size="sm" color="neutral" variant="ghost" (click)="addCondition()"><rm-icon name="plus" class="size-4" />Another condition</button>
            @if (f.conditions.length > 1) {
              <div class="flex items-center gap-3 text-sm" role="radiogroup" aria-label="How the conditions go together">
                <label class="flex items-center gap-1.5"><input type="radio" name="match" value="all" [(ngModel)]="f.match">All of these</label>
                <label class="flex items-center gap-1.5"><input type="radio" name="match" value="any" [(ngModel)]="f.match">Any of these</label>
              </div>
            }
          </div>
        </fieldset>

        <fieldset class="flex flex-col gap-2">
          <legend class="mb-1 text-sm font-medium">Then</legend>
          <div class="flex items-center gap-2">
            <label class="w-32 shrink-0 ${CHECK}"><input type="checkbox" name="move" [(ngModel)]="f.move" (ngModelChange)="$event && (f.remove = false)">Move to</label>
            <rm-select class="flex-1" [value]="f.moveTo" [options]="mailboxes()" label="Mailbox to move to" placeholder="Choose a mailbox" (valueChange)="setMailbox('moveTo', $event)" />
          </div>
          <div class="flex items-center gap-2">
            <label class="w-32 shrink-0 ${CHECK}"><input type="checkbox" name="label" [(ngModel)]="f.label">Label</label>
            <rm-select class="flex-1" [value]="f.labelWith" [options]="mailboxes()" label="Mailbox to label with" placeholder="Choose a mailbox" (valueChange)="setMailbox('labelWith', $event)" />
          </div>
          <label class="${CHECK}"><input type="checkbox" name="read" [(ngModel)]="f.read">Mark as read</label>
          <div class="flex items-center gap-2">
            <label class="w-32 shrink-0 ${CHECK}"><input type="checkbox" name="forward" [(ngModel)]="f.forward">Forward to</label>
            <input rmInput class="min-w-0 flex-1" name="forwardTo" [disabled]="!f.forward" inputmode="email" autocomplete="off" aria-label="Address to forward to" placeholder="someone@example.org" [(ngModel)]="f.forwardTo">
          </div>
          <label class="${CHECK}"><input type="checkbox" name="remove" [(ngModel)]="f.remove" (ngModelChange)="$event && (f.move = false)">Delete it</label>
        </fieldset>
      </form>
      <ng-container modal-footer>
        <button rmBtn type="button" variant="ghost" color="neutral" (click)="editing.set(null)">Cancel</button>
        <button rmBtn type="submit" form="rule-form" [disabled]="busy()">{{ editing() === -1 ? 'Add' : 'Save' }}</button>
      </ng-container>
    </rm-modal>
  `,
})
export class FiltersPage implements OnInit {
  private readonly toaster = inject(Toaster)
  protected readonly fields = fields
  protected readonly describe = sieveRules.describe

  protected readonly loadError = signal('')
  protected readonly loading = signal(true)
  protected readonly scripts = signal<SieveScript[]>([])
  // The script as the server has it, with LF line ends.
  protected readonly text = signal('')
  protected readonly draft = signal('')
  protected readonly tab = signal<'rules' | 'script'>('rules')
  protected readonly busy = signal(false)
  protected readonly saveError = signal('')

  // The script the page is about: the active one, else the one it made
  // before, else the first.
  protected readonly current = computed(() => this.scripts().find((s) => s.isActive) ?? this.scripts().find((s) => s.name === NAME) ?? this.scripts()[0] ?? null)
  protected readonly enabled = computed(() => this.current()?.isActive ?? false)
  protected readonly rules = computed<Rule[] | null>(() => (this.current() ? sieveRules.parse(this.text()) : []))

  // The mailboxes a rule can file into, by the name Sieve knows them by:
  // the levels joined by "/", the inbox as INBOX.
  protected readonly mailboxes = computed(() => {
    const all = store.state.mailboxes
    const path = (id: string, depth = 0): string => {
      const m = all[id]
      if (!m || depth > 20) return ''
      const own = m.role === 'inbox' && !m.parentId ? 'INBOX' : m.name
      return m.parentId ? `${path(m.parentId, depth + 1)}/${own}` : own
    }
    return Object.values(all)
      .filter((m) => m.role !== 'drafts' && m.role !== 'sent')
      .map((m) => ({ label: path(m.id) === 'INBOX' ? 'Inbox' : path(m.id), value: path(m.id) }))
      .sort((a, b) => a.label.localeCompare(b.label))
  })

  // The index of the rule in the dialog; -1 for a new one.
  protected readonly editing = signal<number | null>(null)
  // The form is one object that the fields write into; a change of its
  // shape (a condition more or less) makes a new one.
  protected readonly form = signal<Form>(blank())
  protected readonly formError = signal('')

  ngOnInit(): void {
    document.title = 'Filters · robinmail'
    void this.load(true)
  }

  // load reads the scripts. first is the load of the page itself: only then
  // does a script that is not rules open as text, so that a save does not
  // change the tab under the user's hands.
  protected async load(first = false): Promise<void> {
    this.loadError.set('')
    this.loading.set(true)
    try {
      await loadAccount()
      // The mailboxes a rule can name, as they are now: this page has no
      // push of its own.
      await store.refresh()
      const b = client.batch()
      const get = b.call('SieveScript/get', { ids: null })
      this.scripts.set((await b.send()).get(get).list)
      let text = ''
      const cur = this.current()
      if (cur) {
        const res = await fetch(client.downloadUrl(cur.blobId, 'filters.sieve', 'application/sieve'), { credentials: 'same-origin' })
        if (!res.ok) throw new Error(`The script could not be read (HTTP ${res.status}).`)
        text = (await res.text()).replace(/\r\n/g, '\n')
      }
      this.text.set(text)
      this.draft.set(text)
      if (first && this.rules() === null) this.tab.set('script')
    } catch (e) {
      if (auth.account) this.loadError.set(e instanceof Error ? e.message : String(e))
    } finally {
      this.loading.set(false)
    }
  }

  // save stores a script as the account's filter. Without a script yet it
  // makes one and turns it on; an existing one keeps being on or off.
  private async save(script: string): Promise<boolean> {
    if (this.busy()) return false
    this.busy.set(true)
    this.saveError.set('')
    try {
      const up = await client.upload(new Blob([script.replace(/\r?\n/g, '\r\n')]), 'application/sieve')
      const b = client.batch()
      const cur = this.current()
      const set = cur
        ? b.call('SieveScript/set', { update: { [cur.id]: { blobId: up.blobId } } })
        : b.call('SieveScript/set', { create: { filters: { name: NAME, blobId: up.blobId } }, onSuccessActivateScript: '#filters' })
      const r = (await b.send()).get(set)
      const failed = cur ? r.notUpdated?.[cur.id] : r.notCreated?.['filters']
      if (failed) {
        this.saveError.set(explain(failed))
        return false
      }
      await this.load()
      return true
    } catch (e) {
      this.saveError.set(e instanceof MethodError ? e.description || e.type : e instanceof Error ? e.message : String(e))
      return false
    } finally {
      this.busy.set(false)
    }
  }

  protected async setEnabled(on: boolean): Promise<void> {
    const cur = this.current()
    if (!cur || this.busy()) return
    this.busy.set(true)
    this.saveError.set('')
    try {
      const b = client.batch()
      const set = b.call('SieveScript/set', on ? { onSuccessActivateScript: cur.id } : { onSuccessDeactivateScript: true })
      ;(await b.send()).get(set)
      await this.load()
      this.toaster.add({ title: on ? 'Filters are on' : 'Filters are off', description: on ? undefined : 'New mail goes to the Inbox as it is.', icon: 'check' })
    } catch (e) {
      this.saveError.set(e instanceof Error ? e.message : String(e))
    } finally {
      this.busy.set(false)
    }
  }

  protected showScript(): void {
    this.tab.set('script')
    this.draft.set(this.text())
  }

  // --- the rules ---

  protected edit(index: number): void {
    this.formError.set('')
    const r = index >= 0 ? this.rules()?.[index] : undefined
    const f = blank()
    if (r) {
      f.name = r.name
      f.match = r.match
      f.conditions = r.conditions.map((c) => ({ ...c }))
      for (const a of r.actions) {
        if (a.kind === 'move') [f.move, f.moveTo] = [true, a.mailbox]
        if (a.kind === 'label') [f.label, f.labelWith] = [true, a.mailbox]
        if (a.kind === 'read') f.read = true
        if (a.kind === 'forward') [f.forward, f.forwardTo] = [true, a.address]
        if (a.kind === 'delete') f.remove = true
      }
    }
    this.form.set(f)
    this.editing.set(index)
  }

  protected setField(n: number, field: string): void {
    this.form.update((f) => ({ ...f, conditions: f.conditions.map((c, i) => (i === n ? { ...c, field: field as Field } : c)) }))
  }
  protected setMailbox(which: 'moveTo' | 'labelWith', mailbox: string): void {
    // Choosing a mailbox says the action is wanted.
    this.form.update((f) => (which === 'moveTo' ? { ...f, moveTo: mailbox, move: true, remove: false } : { ...f, labelWith: mailbox, label: true }))
  }
  protected addCondition(): void {
    this.form.update((f) => ({ ...f, conditions: [...f.conditions, { field: 'subject', text: '' }] }))
  }
  protected removeCondition(n: number): void {
    this.form.update((f) => ({ ...f, conditions: f.conditions.filter((_, i) => i !== n) }))
  }

  private fromForm(f: Form): Rule {
    const actions: sieveRules.Action[] = []
    if (f.read) actions.push({ kind: 'read' })
    if (f.label) actions.push({ kind: 'label', mailbox: f.labelWith })
    if (f.forward) actions.push({ kind: 'forward', address: f.forwardTo.trim() })
    if (f.move) actions.push({ kind: 'move', mailbox: f.moveTo })
    if (f.remove) actions.push({ kind: 'delete' })
    return { name: f.name.trim(), match: f.match, conditions: f.conditions.map((c) => ({ field: c.field, text: c.text.trim() })), actions }
  }

  private async saveRules(next: Rule[], done: string): Promise<void> {
    if (await this.save(sieveRules.generate(next))) this.toaster.add({ title: done, icon: 'check' })
  }

  protected async submitRule(): Promise<void> {
    const rule = this.fromForm(this.form())
    this.formError.set(sieveRules.problem(rule))
    if (this.formError()) return
    const next = [...(this.rules() ?? [])]
    const at = this.editing()
    if (at !== null && at >= 0) next[at] = rule
    else next.push(rule)
    const isNew = at === -1
    this.editing.set(null)
    await this.saveRules(next, isNew ? `Added “${rule.name}”` : `Saved “${rule.name}”`)
  }

  protected removeRule(index: number): void {
    const list = this.rules() ?? []
    void this.saveRules(list.filter((_, i) => i !== index), `Removed “${list[index]?.name}”`)
  }

  protected moveRule(index: number, by: -1 | 1): void {
    const next = [...(this.rules() ?? [])]
    const to = index + by
    if (to < 0 || to >= next.length) return
    ;[next[index], next[to]] = [next[to]!, next[index]!]
    void this.saveRules(next, 'Order changed')
  }

  // --- the script as text ---

  protected async saveScript(): Promise<void> {
    if (await this.save(this.draft())) this.toaster.add({ title: 'Script saved', icon: 'check' })
  }

  // Replace a script written by hand with an empty list of rules.
  protected async startOver(): Promise<void> {
    if (await this.save(sieveRules.generate([]))) this.tab.set('rules')
  }
}
