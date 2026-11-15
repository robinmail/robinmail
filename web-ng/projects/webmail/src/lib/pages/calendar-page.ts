// The calendar: a day, a week or a month of the calendars the user sees,
// theirs and those shared with them; an editor for events with repetition,
// guests and a reminder; the invitations waiting for an answer.

import { ChangeDetectionStrategy, Component, type OnDestroy, type OnInit, computed, inject, signal } from '@angular/core'
import { FormsModule } from '@angular/forms'
import { ActivatedRoute, Router, RouterLink } from '@angular/router'
import {
  type CalendarView, type EventDraft, type Occurrence, addDays, addMonths, browserZone, describeRule, layoutDay, localDate,
  monthGrid, parseLocal, sameDay, startOfDay, startOfWeek,
} from '@robinmail/jmap'
import { calendar } from '../app'
import { AppSwitcher } from '../components/app-switcher'
import { ALERT, Btn, Input, LABEL, SKELETON } from '../ui/controls'
import { Icon } from '../ui/icon'
import { Modal } from '../ui/modal'
import { Toaster } from '../ui/toast'

type View = 'day' | 'week' | 'month'
const HOUR_PX = 48
const WEEKDAYS = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun']
const STATUS: Record<string, string> = { accepted: 'Going', tentative: 'Maybe', declined: 'Not going', 'needs-action': 'Not answered' }

interface Editing {
  occurrence: Occurrence | null
  draft: EventDraft
  scope: 'one' | 'all'
}

@Component({
  selector: 'rm-calendar-page',
  imports: [FormsModule, RouterLink, AppSwitcher, Btn, Input, Icon, Modal],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'block h-full' },
  template: `
    <div class="flex h-full overflow-hidden">
      @if (drawer()) {
        <div class="fixed inset-0 z-30 bg-black/40 lg:hidden" (click)="drawer.set(false)"></div>
      }
      <aside class="fixed inset-y-0 left-0 z-40 w-72 max-w-[85vw] flex-col border-r border-default bg-muted lg:static lg:z-auto lg:flex lg:w-64 lg:shrink-0" [class]="drawer() ? 'flex' : 'hidden'">
        <div class="flex h-(--ui-header-height) shrink-0 items-center px-4">
          <a routerLink="/mail" class="flex items-center gap-2 rounded focus-visible:outline-2 focus-visible:outline-primary">
            <img src="/favicon.svg" alt="" class="size-6">
            <span class="font-semibold tracking-tight text-highlighted">robinmail</span>
          </a>
        </div>
        <div class="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-3 pb-3">
          <rm-app-switcher />
          <button rmBtn type="button" [block]="true" data-testid="new-event" [disabled]="!store.state.ready" (click)="newEvent(defaultStart())">
            <rm-icon name="plus" class="size-4" />New event
          </button>
          <section aria-label="Calendars">
            <h2 class="mb-1 px-1 text-xs font-medium uppercase tracking-wide text-dimmed">Calendars</h2>
            @if (!store.state.ready) {
              <div class="h-7 w-full ${SKELETON}"></div>
            }
            <ul class="flex flex-col gap-0.5">
              @for (c of store.state.calendars; track store.key(c)) {
                <li class="group flex items-center gap-2 rounded-md px-1 py-1 hover:bg-elevated/50" data-testid="calendar-item">
                  <input type="checkbox" class="size-4 rounded" [style.accent-color]="c.color" [checked]="store.visible({ accountId: c.accountId, calendarId: c.calendar.id })"
                    [attr.aria-label]="'Show ' + c.calendar.name" (change)="store.toggle(c)">
                  <span class="min-w-0 flex-1 truncate text-sm" [title]="c.own ? c.calendar.name : c.calendar.name + ' (' + c.owner + ')'">
                    {{ c.calendar.name }}@if (!c.own) {<span class="text-dimmed"> · {{ c.owner }}</span>}
                  </span>
                  <button type="button" class="rounded p-1 text-dimmed hover:text-highlighted lg:opacity-0 lg:group-hover:opacity-100 focus-visible:opacity-100"
                    [attr.aria-label]="'Settings of ' + c.calendar.name" (click)="openCalendar(c)">
                    <rm-icon name="settings" class="size-3.5" />
                  </button>
                </li>
              }
            </ul>
            <button rmBtn type="button" variant="ghost" color="neutral" size="sm" class="mt-1 justify-start !text-muted" (click)="openCalendar(null)">
              <rm-icon name="plus" class="size-4" />New calendar
            </button>
          </section>
          @if (store.state.invitations.length) {
            <section aria-label="Invitations">
              <h2 class="mb-1 px-1 text-xs font-medium uppercase tracking-wide text-dimmed">Invitations</h2>
              <ul class="flex flex-col gap-2">
                @for (o of store.state.invitations; track o.id) {
                  <li class="rounded-md bg-default p-2 text-sm ring ring-default" data-testid="invitation">
                    <button type="button" class="block w-full text-left" (click)="open(o)">
                      <span class="block truncate font-medium text-highlighted">{{ o.event.title || '(no title)' }}</span>
                      <span class="block text-xs text-muted">{{ when(o) }}</span>
                    </button>
                    <div class="mt-2 flex gap-1">
                      <button rmBtn type="button" size="xs" color="success" variant="subtle" (click)="respond(o, 'accepted')">Accept</button>
                      <button rmBtn type="button" size="xs" color="neutral" variant="subtle" (click)="respond(o, 'tentative')">Maybe</button>
                      <button rmBtn type="button" size="xs" color="error" variant="subtle" (click)="respond(o, 'declined')">Decline</button>
                    </div>
                  </li>
                }
              </ul>
            </section>
          }
        </div>
      </aside>

      <main class="flex min-w-0 flex-1 flex-col">
        <div class="flex h-(--ui-header-height) shrink-0 items-center gap-1.5 border-b border-default px-3 sm:px-4">
          <button rmBtn type="button" variant="ghost" color="neutral" class="lg:hidden" aria-label="Calendars menu" (click)="drawer.set(true)">
            <rm-icon name="menu" class="size-4" />
          </button>
          <button rmBtn type="button" variant="outline" color="neutral" size="sm" (click)="go('today')">Today</button>
          <button rmBtn type="button" variant="ghost" color="neutral" size="sm" aria-label="Previous" data-testid="prev" (click)="go(-1)"><rm-icon name="chevron-left" class="size-4" /></button>
          <button rmBtn type="button" variant="ghost" color="neutral" size="sm" aria-label="Next" data-testid="next" (click)="go(1)"><rm-icon name="chevron-right" class="size-4" /></button>
          <h1 class="truncate font-semibold text-highlighted" data-testid="calendar-title">{{ title() }}</h1>
          <div class="ml-auto flex items-center gap-1">
            @if (store.state.loading) {
              <rm-icon name="loader-circle" class="size-4 animate-spin text-dimmed" />
            }
            <div class="flex rounded-md ring ring-inset ring-accented" role="group" aria-label="View">
              @for (v of views; track v) {
                <button type="button" class="px-2.5 py-1 text-xs font-medium first:rounded-l-md last:rounded-r-md" [attr.data-testid]="'view-' + v"
                  [class]="view() === v ? 'bg-elevated text-highlighted' : 'text-muted hover:text-highlighted'" [attr.aria-pressed]="view() === v" (click)="setView(v)">
                  {{ v === 'day' ? 'Day' : v === 'week' ? 'Week' : 'Month' }}
                </button>
              }
            </div>
            <label rmBtn variant="ghost" color="neutral" size="sm" class="cursor-pointer" title="Import an .ics file">
              <rm-icon name="upload" class="size-4" /><span class="max-sm:hidden">Import</span>
              <input type="file" accept=".ics,text/calendar" class="sr-only" data-testid="import-ics" (change)="importFile($event)">
            </label>
          </div>
        </div>

        @if (store.state.error) {
          <div class="${ALERT} m-3" role="alert"><rm-icon name="circle-alert" class="mt-0.5 size-4" /><span>{{ store.state.error }}</span></div>
        }

        @if (view() === 'month') {
          <div class="grid shrink-0 grid-cols-7 border-b border-default text-xs font-medium text-muted">
            @for (d of weekdays; track d) {
              <div class="px-2 py-1.5">{{ d }}</div>
            }
          </div>
          <div class="grid min-h-0 flex-1 grid-cols-7 grid-rows-6" data-testid="month-grid">
            @for (day of monthDays(); track day.getTime()) {
              <div class="min-h-0 overflow-hidden border-b border-r border-default p-1" [class.bg-muted]="day.getMonth() !== anchor().getMonth()"
                data-testid="calendar-day" [attr.data-date]="iso(day)" (click)="newEvent(atNine(day))">
                <div class="mb-0.5 text-right text-xs">
                  <span class="inline-flex size-5 items-center justify-center rounded-full" [class]="isToday(day) ? 'bg-primary text-inverted font-semibold' : 'text-muted'">{{ day.getDate() }}</span>
                </div>
                @for (o of on(day).slice(0, 3); track o.id) {
                  <button type="button" class="mb-0.5 flex w-full items-center gap-1 truncate rounded px-1 text-left text-xs hover:bg-elevated" data-testid="event-chip"
                    (click)="$event.stopPropagation(); open(o)">
                    <span class="size-2 shrink-0 rounded-full" [style.background]="o.color"></span>
                    @if (!o.allDay) {<span class="text-dimmed">{{ time(o.start) }}</span>}
                    <span class="truncate" [class.line-through]="o.myStatus === 'declined'">{{ o.event.title || '(no title)' }}</span>
                  </button>
                }
                @if (on(day).length > 3) {
                  <button type="button" class="px-1 text-xs text-muted hover:text-highlighted" (click)="$event.stopPropagation(); showDay(day)">{{ on(day).length - 3 }} more</button>
                }
              </div>
            }
          </div>
        } @else {
          <div class="flex shrink-0 border-b border-default">
            <div class="w-14 shrink-0"></div>
            @for (day of days(); track day.getTime()) {
              <div class="min-w-0 flex-1 border-l border-default px-2 py-1.5 text-xs">
                <div class="text-muted">{{ weekday(day) }}</div>
                <div class="text-lg leading-tight" [class]="isToday(day) ? 'font-semibold text-primary' : 'text-highlighted'">{{ day.getDate() }}</div>
                @for (o of allDayOn(day); track o.id) {
                  <button type="button" class="mt-0.5 block w-full truncate rounded px-1 text-left text-xs text-white" [style.background]="o.color" data-testid="event-chip" (click)="open(o)">
                    {{ o.event.title || '(no title)' }}
                  </button>
                }
              </div>
            }
          </div>
          <div class="relative min-h-0 flex-1 overflow-y-auto" #scroller data-testid="time-grid">
            <div class="flex" [style.height.px]="24 * hourPx">
              <div class="w-14 shrink-0">
                @for (h of hours; track h) {
                  <div class="relative text-right text-[10px] text-dimmed" [style.height.px]="hourPx"><span class="absolute -top-1.5 right-1">{{ h ? pad(h) + ':00' : '' }}</span></div>
                }
              </div>
              @for (day of days(); track day.getTime()) {
                <div class="relative min-w-0 flex-1 border-l border-default" data-testid="calendar-day" [attr.data-date]="iso(day)">
                  @for (h of hours; track h) {
                    <div class="border-b border-default/60 hover:bg-elevated/40" [style.height.px]="hourPx" (click)="newEvent(atHour(day, h))"></div>
                  }
                  @for (p of placed(day); track p.item.id) {
                    <button type="button" class="absolute overflow-hidden rounded px-1 py-0.5 text-left text-xs text-white shadow-sm ring-1 ring-default/40" data-testid="event-chip"
                      [style.top.px]="p.top * hourPx / 60" [style.height.px]="max(18, p.height * hourPx / 60 - 1)"
                      [style.left]="(p.column / p.columns * 100) + '%'" [style.width]="'calc(' + (100 / p.columns) + '% - 2px)'"
                      [style.background]="p.item.color" [style.opacity]="p.item.myStatus === 'declined' ? 0.5 : 1" (click)="open(p.item)">
                      <span class="block truncate font-medium">{{ p.item.event.title || '(no title)' }}</span>
                      <span class="block truncate opacity-85">{{ time(p.item.start) }}–{{ time(p.item.end) }}</span>
                    </button>
                  }
                  @if (isToday(day)) {
                    <div class="pointer-events-none absolute inset-x-0 h-0.5 bg-error" [style.top.px]="nowMinutes() * hourPx / 60"></div>
                  }
                </div>
              }
            </div>
          </div>
        }
      </main>
    </div>

    <!-- The event editor. -->
    <rm-modal [open]="editing() !== null" (openChange)="$event || editing.set(null)" [wide]="true" [title]="editorTitle()">
      @let e = editing();
      @if (e) {
        @if (error()) {
          <div class="${ALERT} mb-4" role="alert"><rm-icon name="circle-alert" class="mt-0.5 size-4" /><span>{{ error() }}</span></div>
        }
        @if (e.occurrence && !e.occurrence.isOrigin) {
          <!-- An invitation: the organizer's event, the user's answer. -->
          <div class="flex flex-col gap-3 text-sm" data-testid="invitation-detail">
            <p class="text-base font-medium text-highlighted">{{ e.occurrence.event.title || '(no title)' }}</p>
            <p class="flex items-center gap-2 text-muted"><rm-icon name="clock" class="size-4" />{{ when(e.occurrence) }}</p>
            @if (location(e.occurrence)) {
              <p class="flex items-center gap-2 text-muted"><rm-icon name="map-pin" class="size-4" />{{ location(e.occurrence) }}</p>
            }
            <p class="flex items-center gap-2 text-muted"><rm-icon name="user" class="size-4" />From {{ organizer(e.occurrence) }}</p>
            @if (e.occurrence.event.description) {
              <p class="whitespace-pre-wrap">{{ e.occurrence.event.description }}</p>
            }
            <p>Your answer: <strong data-testid="my-status">{{ statusText(e.occurrence.myStatus) }}</strong></p>
            <div class="flex gap-2">
              <button rmBtn type="button" color="success" variant="subtle" data-testid="accept" (click)="respond(e.occurrence, 'accepted')"><rm-icon name="check" class="size-4" />Accept</button>
              <button rmBtn type="button" color="neutral" variant="subtle" (click)="respond(e.occurrence, 'tentative')">Maybe</button>
              <button rmBtn type="button" color="error" variant="subtle" data-testid="decline" (click)="respond(e.occurrence, 'declined')"><rm-icon name="x" class="size-4" />Decline</button>
            </div>
          </div>
        } @else {
          <form id="event-form" class="flex flex-col gap-4" (ngSubmit)="save()">
            <label class="${LABEL}"><span>Title</span>
              <input rmInput name="title" autofocus data-testid="event-title" [(ngModel)]="e.draft.title" maxlength="500">
            </label>
            <div class="grid gap-4 sm:grid-cols-2">
              <label class="${LABEL}"><span>Calendar</span>
                <select rmInput name="calendar" data-testid="event-calendar" [ngModel]="store.key(e.draft)" (ngModelChange)="setCalendar(e, $event)">
                  @for (c of writable(); track store.key(c)) {
                    <option [value]="store.key({ accountId: c.accountId, calendar: c.calendar })">{{ c.calendar.name }}{{ c.own ? '' : ' · ' + c.owner }}</option>
                  }
                </select>
              </label>
              <label class="flex items-center gap-2 self-end pb-1.5 text-sm"><input type="checkbox" name="allDay" class="size-4" data-testid="event-allday" [(ngModel)]="e.draft.allDay">All day</label>
            </div>
            <div class="grid gap-4 sm:grid-cols-2">
              <label class="${LABEL}"><span>Starts</span>
                <input rmInput name="start" [type]="e.draft.allDay ? 'date' : 'datetime-local'" data-testid="event-start" [ngModel]="field(e.draft.start, e.draft.allDay)" (ngModelChange)="setStart(e, $event)">
              </label>
              <label class="${LABEL}"><span>Ends</span>
                <input rmInput name="end" [type]="e.draft.allDay ? 'date' : 'datetime-local'" data-testid="event-end" [ngModel]="field(endShown(e.draft), e.draft.allDay)" (ngModelChange)="setEnd(e, $event)">
              </label>
            </div>
            <label class="${LABEL}"><span>Location</span>
              <input rmInput name="location" data-testid="event-location" [(ngModel)]="e.draft.location">
            </label>
            <div class="grid gap-4 sm:grid-cols-2">
              <label class="${LABEL}"><span>Repeats</span>
                <select rmInput name="repeat" data-testid="event-repeat" [(ngModel)]="e.draft.repeat">
                  <option value="">Does not repeat</option>
                  <option value="daily">Every day</option>
                  <option value="weekly">Every week</option>
                  <option value="monthly">Every month</option>
                  <option value="yearly">Every year</option>
                </select>
              </label>
              @if (e.draft.repeat) {
                <label class="${LABEL}"><span>Times (empty: no end)</span>
                  <input rmInput name="count" type="number" min="1" max="1000" data-testid="event-count" [(ngModel)]="e.draft.count">
                </label>
              }
            </div>
            <label class="${LABEL}"><span>Guests (addresses, separated by commas)</span>
              <input rmInput name="attendees" data-testid="event-guests" [ngModel]="e.draft.attendees.join(', ')" (ngModelChange)="setGuests(e, $event)" placeholder="name@example.com">
            </label>
            @if (e.draft.attendees.length) {
              <p class="-mt-2 text-xs text-muted">Saving sends them an invitation by email; guests of this server see it in their calendar at once.</p>
            }
            @if (e.occurrence && e.occurrence.event.participants) {
              <ul class="-mt-2 flex flex-wrap gap-1 text-xs">
                @for (p of participants(e.occurrence); track p.address) {
                  <li class="rounded bg-elevated px-1.5 py-0.5" [title]="p.address">{{ p.label }}: {{ statusText(p.status) }}</li>
                }
              </ul>
            }
            <div class="grid gap-4 sm:grid-cols-2">
              <label class="${LABEL}"><span>Reminder</span>
                <select rmInput name="alert" [ngModel]="e.draft.alertMinutes === null ? 'none' : '' + e.draft.alertMinutes" (ngModelChange)="e.draft.alertMinutes = $event === 'none' ? null : +$event">
                  <option value="none">None</option>
                  <option value="0">At the start</option>
                  <option value="10">10 minutes before</option>
                  <option value="30">30 minutes before</option>
                  <option value="60">1 hour before</option>
                  <option value="1440">1 day before</option>
                </select>
              </label>
              @if (e.occurrence?.recurring) {
                <label class="${LABEL}"><span>Change</span>
                  <select rmInput name="scope" data-testid="event-scope" [(ngModel)]="e.scope">
                    <option value="one">Only this event</option>
                    <option value="all">All events of the series</option>
                  </select>
                </label>
              }
            </div>
            <label class="${LABEL}"><span>Notes</span>
              <textarea rmInput name="description" rows="3" [(ngModel)]="e.draft.description"></textarea>
            </label>
            @if (e.occurrence && e.occurrence.recurring && e.occurrence.event.recurrenceRule) {
              <p class="text-xs text-muted">{{ describe(e.occurrence) }}</p>
            }
          </form>
        }
      }
      <ng-container modal-footer>
        @if (e && e.occurrence && e.occurrence.isOrigin) {
          <button rmBtn type="button" variant="ghost" color="error" class="mr-auto" data-testid="event-delete" [disabled]="busy()" (click)="remove()">
            <rm-icon name="trash-2" class="size-4" />Delete
          </button>
        }
        <button rmBtn type="button" variant="ghost" color="neutral" (click)="editing.set(null)">{{ e && e.occurrence && !e.occurrence.isOrigin ? 'Close' : 'Cancel' }}</button>
        @if (!e || !e.occurrence || e.occurrence.isOrigin) {
          <button rmBtn type="submit" form="event-form" data-testid="event-save" [disabled]="busy()">Save</button>
        }
      </ng-container>
    </rm-modal>

    <!-- A calendar's settings: name, colour, sharing, export. -->
    <rm-modal [open]="calendarDialog() !== null" (openChange)="$event || calendarDialog.set(null)" [title]="calendarDialog()?.view ? 'Calendar settings' : 'New calendar'">
      @let d = calendarDialog();
      @if (d) {
        <form id="calendar-form" class="flex flex-col gap-4" (ngSubmit)="saveCalendar()">
          @if (error()) {
            <div class="${ALERT}" role="alert"><rm-icon name="circle-alert" class="mt-0.5 size-4" /><span>{{ error() }}</span></div>
          }
          @if (!d.view || d.view.own) {
            <label class="${LABEL}"><span>Name</span><input rmInput name="name" data-testid="calendar-name" [(ngModel)]="d.name" required maxlength="255"></label>
            <label class="${LABEL}"><span>Colour</span><input rmInput type="color" name="color" class="!h-9 !p-1" [(ngModel)]="d.color"></label>
          } @else {
            <p class="text-sm">{{ d.view.calendar.name }}, shared by {{ d.view.owner }}{{ d.view.calendar.myRights.mayWriteAll ? ', which you may change' : ', to read' }}.</p>
          }
          @if (d.view && d.view.own) {
            <fieldset class="flex flex-col gap-2">
              <legend class="mb-1 text-sm font-medium">Shared with</legend>
              @for (s of d.shares; track s.id) {
                <div class="flex items-center gap-2 text-sm" data-testid="share-row">
                  <span class="min-w-0 flex-1 truncate">{{ s.email }}</span>
                  <span class="text-muted">{{ s.rights === 'write' ? 'may change' : 'may read' }}</span>
                  <button rmBtn type="button" size="xs" variant="ghost" color="neutral" [attr.aria-label]="'Stop sharing with ' + s.email" (click)="unshare(s.email)"><rm-icon name="x" class="size-3.5" /></button>
                </div>
              }
              <div class="flex gap-2">
                <input rmInput name="shareEmail" placeholder="name@this-server" data-testid="share-email" [(ngModel)]="d.shareEmail">
                <select rmInput name="shareRights" class="!w-32" [(ngModel)]="d.shareRights"><option value="read">May read</option><option value="write">May change</option></select>
                <button rmBtn type="button" variant="subtle" data-testid="share-add" (click)="addShare()"><rm-icon name="share-2" class="size-4" />Share</button>
              </div>
            </fieldset>
          }
          @if (d.view) {
            <a rmBtn variant="link" [href]="store.exportURL(d.view)" download><rm-icon name="download" class="size-4" />Download as .ics</a>
          }
        </form>
      }
      <ng-container modal-footer>
        @if (d?.view) {
          <button rmBtn type="button" variant="ghost" color="error" class="mr-auto" [disabled]="busy() || (d && d.view && d.view.calendar.isDefault)" (click)="deleteCalendar()">
            {{ d && d.view && d.view.own ? 'Delete' : 'Remove from my calendars' }}
          </button>
        }
        <button rmBtn type="button" variant="ghost" color="neutral" (click)="calendarDialog.set(null)">Close</button>
        @if (!d || !d.view || d.view.own) {
          <button rmBtn type="submit" form="calendar-form" data-testid="calendar-save" [disabled]="busy()">Save</button>
        }
      </ng-container>
    </rm-modal>

    <!-- Asking which events a delete is for. -->
    <rm-modal [open]="askScope()" (openChange)="$event || askScope.set(false)" title="Delete a repeating event">
      <p class="text-sm">Delete only this event, or every event of the series?</p>
      <ng-container modal-footer>
        <button rmBtn type="button" variant="ghost" color="neutral" (click)="askScope.set(false)">Cancel</button>
        <button rmBtn type="button" variant="outline" color="error" data-testid="delete-one" (click)="doRemove('one')">This event</button>
        <button rmBtn type="button" color="error" data-testid="delete-all" (click)="doRemove('all')">All events</button>
      </ng-container>
    </rm-modal>
  `,
})
export class CalendarPage implements OnInit, OnDestroy {
  protected readonly store = calendar
  private readonly router = inject(Router)
  private readonly route = inject(ActivatedRoute)
  private readonly toaster = inject(Toaster)

  protected readonly views: View[] = ['day', 'week', 'month']
  protected readonly weekdays = WEEKDAYS
  protected readonly hours = Array.from({ length: 24 }, (_, i) => i)
  protected readonly hourPx = HOUR_PX
  protected readonly view = signal<View>('week')
  protected readonly anchor = signal(startOfDay(new Date()))
  protected readonly drawer = signal(false)
  protected readonly editing = signal<Editing | null>(null)
  protected readonly calendarDialog = signal<{ view: CalendarView | null; name: string; color: string; shareEmail: string; shareRights: 'read' | 'write'; shares: { id: string; email: string; rights: string }[] } | null>(null)
  protected readonly askScope = signal(false)
  protected readonly busy = signal(false)
  protected readonly error = signal('')
  protected readonly nowMinutes = signal(this.minutesNow())
  private timer: ReturnType<typeof setInterval> | undefined
  private readonly onFocus = () => this.store.refresh().catch(() => {})

  protected readonly days = computed(() => {
    const a = this.anchor()
    if (this.view() === 'day') return [a]
    const s = startOfWeek(a)
    return Array.from({ length: 7 }, (_, i) => addDays(s, i))
  })
  protected readonly monthDays = computed(() => monthGrid(this.anchor()))
  protected readonly range = computed(() => {
    if (this.view() === 'month') {
      const g = this.monthDays()
      return { from: g[0]!, to: addDays(g[g.length - 1]!, 1) }
    }
    const d = this.days()
    return { from: d[0]!, to: addDays(d[d.length - 1]!, 1) }
  })
  protected readonly title = computed(() => {
    const a = this.anchor()
    if (this.view() === 'month') return a.toLocaleDateString(undefined, { month: 'long', year: 'numeric' })
    if (this.view() === 'day') return a.toLocaleDateString(undefined, { weekday: 'long', day: 'numeric', month: 'long', year: 'numeric' })
    const d = this.days()
    const first = d[0]!, last = d[6]!
    return first.getMonth() === last.getMonth()
      ? `${first.getDate()}–${last.getDate()} ${last.toLocaleDateString(undefined, { month: 'long', year: 'numeric' })}`
      : `${first.toLocaleDateString(undefined, { day: 'numeric', month: 'short' })} – ${last.toLocaleDateString(undefined, { day: 'numeric', month: 'short', year: 'numeric' })}`
  })
  protected readonly shown = computed(() => this.store.state.occurrences.filter((o) => this.store.visible(o)))
  protected readonly writable = computed(() => this.store.state.calendars.filter((c) => c.calendar.myRights.mayWriteAll))
  protected readonly editorTitle = computed(() => {
    const e = this.editing()
    if (!e) return ''
    if (e.occurrence && !e.occurrence.isOrigin) return 'Invitation'
    return e.occurrence ? 'Edit event' : 'New event'
  })

  ngOnInit(): void {
    const q = this.route.snapshot.queryParamMap
    const v = q.get('view')
    if (v === 'day' || v === 'week' || v === 'month') this.view.set(v)
    const d = q.get('date') ? parseLocal(q.get('date')!) : null
    if (d) this.anchor.set(startOfDay(d))
    this.start().catch((e) => (this.store.state.error = (e as Error).message))
    window.addEventListener('focus', this.onFocus)
    this.timer = setInterval(() => {
      this.nowMinutes.set(this.minutesNow())
      this.store.refresh().catch(() => {})
    }, 60_000)
  }

  ngOnDestroy(): void {
    window.removeEventListener('focus', this.onFocus)
    clearInterval(this.timer)
  }

  private async start(): Promise<void> {
    await this.store.load()
    await this.reload()
    await this.store.loadInvitations()
    setTimeout(() => document.querySelector('[data-testid="time-grid"]')?.scrollTo({ top: 7 * HOUR_PX }), 0)
  }

  private async reload(): Promise<void> {
    const r = this.range()
    await this.store.loadRange(r.from, r.to)
  }

  private minutesNow(): number {
    const n = new Date()
    return n.getHours() * 60 + n.getMinutes()
  }

  private syncUrl(): void {
    this.router.navigate([], { queryParams: { view: this.view(), date: localDate(this.anchor()) }, replaceUrl: true })
  }

  protected setView(v: View): void {
    this.view.set(v)
    this.syncUrl()
    this.reload()
  }

  protected go(step: number | 'today'): void {
    const a = this.anchor()
    if (step === 'today') this.anchor.set(startOfDay(new Date()))
    else if (this.view() === 'month') this.anchor.set(addMonths(a, step))
    else this.anchor.set(addDays(a, step * (this.view() === 'week' ? 7 : 1)))
    this.syncUrl()
    this.reload()
  }

  protected showDay(day: Date): void {
    this.anchor.set(day)
    this.setView('day')
  }

  protected on(day: Date): Occurrence[] {
    const s = day.getTime(), e = addDays(day, 1).getTime()
    return this.shown().filter((o) => o.start.getTime() < e && (o.end.getTime() > s || (o.end.getTime() === o.start.getTime() && o.start.getTime() >= s)))
  }

  protected allDayOn(day: Date): Occurrence[] {
    return this.on(day).filter((o) => o.allDay)
  }

  protected placed(day: Date) {
    return layoutDay(day, this.shown().filter((o) => !o.allDay))
  }

  protected isToday(d: Date): boolean {
    return sameDay(d, new Date())
  }
  protected iso(d: Date): string {
    return localDate(d)
  }
  protected weekday(d: Date): string {
    return d.toLocaleDateString(undefined, { weekday: 'short' })
  }
  protected time(d: Date): string {
    return d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' })
  }
  protected pad(n: number): string {
    return String(n).padStart(2, '0')
  }
  protected max(a: number, b: number): number {
    return Math.max(a, b)
  }

  protected when(o: Occurrence): string {
    const day = o.start.toLocaleDateString(undefined, { weekday: 'short', day: 'numeric', month: 'short', year: 'numeric' })
    if (o.allDay) return day
    return `${day}, ${this.time(o.start)}–${this.time(o.end)}`
  }

  protected location(o: Occurrence): string {
    return Object.values(o.event.locations ?? {})[0]?.name ?? ''
  }

  protected organizer(o: Occurrence): string {
    const org = (o.event.organizerCalendarAddress ?? '').replace(/^mailto:/i, '')
    const p = Object.values(o.event.participants ?? {}).find((x) => x.roles?.['owner'])
    return p?.name ? `${p.name} <${org}>` : org
  }

  protected participants(o: Occurrence): { address: string; label: string; status: string }[] {
    return Object.values(o.event.participants ?? {}).map((p) => {
      const address = (p.calendarAddress ?? p.email ?? '').replace(/^mailto:/i, '')
      return { address, label: p.name || address, status: p.roles?.['owner'] ? 'organizer' : p.participationStatus ?? 'needs-action' }
    })
  }

  protected statusText(s: string | null): string {
    if (s === 'organizer') return 'Organizer'
    return STATUS[s ?? 'needs-action'] ?? s ?? ''
  }

  protected describe(o: Occurrence | null | undefined): string {
    return describeRule(o?.event.recurrenceRule)
  }

  protected defaultStart(): Date {
    const n = new Date()
    const a = this.anchor()
    const base = sameDay(a, n) || this.view() !== 'day' ? n : a
    return new Date(base.getFullYear(), base.getMonth(), base.getDate(), Math.min(base.getHours() + 1, 23), 0)
  }
  protected atNine(day: Date): Date {
    return new Date(day.getFullYear(), day.getMonth(), day.getDate(), 9, 0)
  }
  protected atHour(day: Date, h: number): Date {
    return new Date(day.getFullYear(), day.getMonth(), day.getDate(), h, 0)
  }

  // field writes a Date for a date or datetime-local input.
  protected field(d: Date, allDay: boolean): string {
    const s = localDate(d) + 'T' + this.pad(d.getHours()) + ':' + this.pad(d.getMinutes())
    return allDay ? s.slice(0, 10) : s
  }

  // An all-day event that ends at midnight is shown ending the day before.
  protected endShown(d: EventDraft): Date {
    return d.allDay ? addDays(d.end, -1) : d.end
  }

  protected setStart(e: Editing, v: string): void {
    const d = parseLocal(v)
    if (!d) return
    const len = e.draft.end.getTime() - e.draft.start.getTime()
    e.draft.start = d
    e.draft.end = new Date(d.getTime() + Math.max(len, 0))
    this.editing.set({ ...e })
  }

  protected setEnd(e: Editing, v: string): void {
    const d = parseLocal(v)
    if (!d) return
    e.draft.end = e.draft.allDay ? addDays(d, 1) : d
    this.editing.set({ ...e })
  }

  protected setGuests(e: Editing, v: string): void {
    e.draft.attendees = v.split(/[,;\s]+/).map((s) => s.trim()).filter(Boolean)
  }

  protected setCalendar(e: Editing, key: string): void {
    const [accountId, calendarId] = key.split('/')
    e.draft.accountId = accountId!
    e.draft.calendarId = calendarId!
  }

  protected newEvent(start: Date): void {
    const c = this.writable().find((x) => x.own && x.calendar.isDefault) ?? this.writable()[0]
    if (!c) return
    this.error.set('')
    this.drawer.set(false)
    this.editing.set({
      occurrence: null, scope: 'all',
      draft: {
        title: '', accountId: c.accountId, calendarId: c.calendar.id, allDay: false, start, end: new Date(start.getTime() + 3_600_000),
        timeZone: browserZone(), location: '', description: '', repeat: '', until: null, count: null, attendees: [], alertMinutes: null,
      },
    })
  }

  protected open(o: Occurrence): void {
    this.error.set('')
    const ev = o.event
    const rule = ev.recurrenceRule
    const alert = Object.values(ev.alerts ?? {})[0]?.trigger.offset
    let minutes: number | null = null
    if (alert) {
      const m = /^-?PT?(?:(\d+)D)?T?(?:(\d+)H)?(?:(\d+)M)?/.exec(alert.replace('P', 'P'))
      minutes = m ? (+(m[1] ?? 0)) * 1440 + (+(m[2] ?? 0)) * 60 + (+(m[3] ?? 0)) : 0
    }
    const attendees = Object.values(ev.participants ?? {}).filter((p) => !this.store.isMe(p)).map((p) => (p.calendarAddress ?? p.email ?? '').replace(/^mailto:/i, ''))
    this.editing.set({
      occurrence: o, scope: o.recurring ? 'one' : 'all',
      draft: {
        title: ev.title ?? '', accountId: o.accountId, calendarId: o.calendarId, allDay: o.allDay, start: o.start, end: o.end,
        timeZone: ev.timeZone ?? browserZone(), location: this.location(o), description: ev.description ?? '',
        repeat: (rule?.frequency as EventDraft['repeat']) ?? '', until: rule?.until ? parseLocal(rule.until) : null, count: rule?.count ?? null,
        attendees, alertMinutes: minutes,
      },
    })
  }

  protected async save(): Promise<void> {
    const e = this.editing()
    if (!e || this.busy()) return
    if (e.draft.end < e.draft.start) {
      this.error.set('The event must end after it starts.')
      return
    }
    this.busy.set(true)
    this.error.set('')
    try {
      if (e.occurrence) await this.store.update(e.occurrence, e.draft, e.scope)
      else await this.store.create(e.draft)
      this.editing.set(null)
      this.toaster.add({ title: e.draft.attendees.length ? 'Saved; the guests were invited.' : 'Saved.' })
    } catch (err) {
      this.error.set((err as Error).message)
    } finally {
      this.busy.set(false)
    }
  }

  protected remove(): void {
    const e = this.editing()
    if (!e?.occurrence) return
    if (e.occurrence.recurring) {
      this.askScope.set(true)
      return
    }
    this.doRemove('all')
  }

  protected async doRemove(scope: 'one' | 'all'): Promise<void> {
    const o = this.editing()?.occurrence
    this.askScope.set(false)
    if (!o) return
    this.busy.set(true)
    try {
      await this.store.remove(o, scope)
      this.editing.set(null)
      this.toaster.add({ title: 'Deleted.' })
    } catch (err) {
      this.error.set((err as Error).message)
    } finally {
      this.busy.set(false)
    }
  }

  protected async respond(o: Occurrence, status: 'accepted' | 'tentative' | 'declined'): Promise<void> {
    try {
      await this.store.respond(o, status)
      this.editing.set(null)
      this.toaster.add({ title: status === 'accepted' ? 'Accepted.' : status === 'declined' ? 'Declined.' : 'Answered maybe.' })
    } catch (err) {
      this.toaster.error((err as Error).message)
    }
  }

  protected async openCalendar(c: CalendarView | null): Promise<void> {
    this.error.set('')
    const shares: { id: string; email: string; rights: string }[] = []
    if (c?.own && c.calendar.shareWith) {
      for (const [id, r] of Object.entries(c.calendar.shareWith)) shares.push({ id, email: id, rights: r.mayWriteAll ? 'write' : 'read' })
    }
    this.calendarDialog.set({ view: c, name: c?.calendar.name ?? '', color: c?.color ?? '#3b82f6', shareEmail: '', shareRights: 'read', shares })
    if (shares.length) {
      try {
        const b = (await import('../app')).client.batch()
        const g = b.call('Principal/get', { ids: shares.map((s) => s.id) })
        const res = (await b.send()).get(g)
        for (const s of shares) s.email = res.list.find((p) => p.id === s.id)?.email ?? s.id
        this.calendarDialog.update((d) => (d ? { ...d, shares: [...shares] } : d))
      } catch {
        // The ids stay.
      }
    }
  }

  protected async saveCalendar(): Promise<void> {
    const d = this.calendarDialog()
    if (!d || this.busy()) return
    if (!d.name.trim()) {
      this.error.set('Give the calendar a name.')
      return
    }
    this.busy.set(true)
    try {
      if (d.view) await this.store.updateCalendar(d.view, { name: d.name.trim(), color: d.color })
      else await this.store.createCalendar(d.name.trim(), d.color)
      this.calendarDialog.set(null)
    } catch (err) {
      this.error.set((err as Error).message)
    } finally {
      this.busy.set(false)
    }
  }

  protected async addShare(): Promise<void> {
    const d = this.calendarDialog()
    if (!d?.view || !d.shareEmail.trim()) return
    this.error.set('')
    try {
      await this.store.share(d.view, d.shareEmail.trim(), d.shareRights)
      const fresh = this.store.calendarOf(d.view.accountId, d.view.calendar.id)
      await this.openCalendar(fresh ?? d.view)
      this.toaster.add({ title: `Shared with ${d.shareEmail.trim()}.` })
    } catch (err) {
      this.error.set((err as Error).message)
    }
  }

  protected async unshare(email: string): Promise<void> {
    const d = this.calendarDialog()
    if (!d?.view) return
    try {
      await this.store.share(d.view, email, null)
      const fresh = this.store.calendarOf(d.view.accountId, d.view.calendar.id)
      await this.openCalendar(fresh ?? d.view)
    } catch (err) {
      this.error.set((err as Error).message)
    }
  }

  protected async deleteCalendar(): Promise<void> {
    const d = this.calendarDialog()
    if (!d?.view || this.busy()) return
    if (!confirm(d.view.own ? `Delete “${d.view.calendar.name}” and all its events?` : `Remove “${d.view.calendar.name}” from your calendars?`)) return
    this.busy.set(true)
    try {
      if (d.view.own) await this.store.deleteCalendar(d.view)
      else {
        // A sharee leaves a shared calendar through CalDAV's DELETE, which
        // ends the share for them.
        await fetch(this.store.exportURL(d.view), { method: 'DELETE', credentials: 'same-origin' })
        await this.store.refresh()
      }
      this.calendarDialog.set(null)
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
    const c = this.writable().find((x) => x.own && x.calendar.isDefault) ?? this.writable()[0]
    if (!c) {
      this.toaster.error('There is no calendar to import into.')
      return
    }
    try {
      const n = await this.store.importICS(file, c)
      this.toaster.add({ title: `${n} ${n === 1 ? 'event' : 'events'} imported into ${c.calendar.name}.` })
    } catch (err) {
      this.toaster.error((err as Error).message)
    }
  }
}
