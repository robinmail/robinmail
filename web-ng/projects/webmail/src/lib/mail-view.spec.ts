import { provideZonelessChangeDetection } from '@angular/core'
import { TestBed } from '@angular/core/testing'
import { Router, provideRouter } from '@angular/router'
import { beforeEach, describe, expect, it } from 'vitest'
import { auth } from './app'
import { MailView } from './mail-view'
import { provideWebmail, webmailRoutes } from './routes'
import { safeNext } from './session'

describe('the route as the state of the mail view', () => {
  let router: Router
  let view: MailView

  beforeEach(() => {
    // Logged in, and asked already: the guard lets every route through.
    auth.checked = true
    auth.account = { username: 'dev@robinmail.test' } as typeof auth.account
    TestBed.configureTestingModule({ providers: [provideZonelessChangeDetection(), provideRouter(webmailRoutes), provideWebmail()] })
    router = TestBed.inject(Router)
    view = TestBed.inject(MailView)
  })

  it('reads the mailbox, the thread and the filter from the address', async () => {
    await router.navigateByUrl('/mail/m1/t9?q=harbour%20fee&from=bert&unread=1&attachment=1')
    expect(view.onMail()).toBe(true)
    expect(view.mailboxId()).toBe('m1')
    expect(view.threadId()).toBe('t9')
    expect(view.spec()).toEqual({ mailboxId: 'm1', text: 'harbour fee', from: 'bert', subject: '', unread: true, flagged: false, attachment: true })
    expect(view.filterCount()).toBe(4)
  })

  it('keeps the page for /mail, /mail/<mailbox> and /mail/<mailbox>/<thread>: one route', async () => {
    await router.navigateByUrl('/mail')
    const first = router.routerState.snapshot.root.firstChild?.firstChild?.routeConfig
    await router.navigateByUrl('/mail/m1/t1')
    expect(router.routerState.snapshot.root.firstChild?.firstChild?.routeConfig).toBe(first)
    expect(view.threadId()).toBe('t1')
    await router.navigateByUrl('/mail/m1/t1/more')
    expect(router.url).toBe('/mail') // not a mail address: the catch-all sends it back
  })

  it('changes the filter in the address and keeps the open thread', async () => {
    await router.navigateByUrl('/mail/m1/t9?unread=1')
    await view.setFilter({ flagged: true, subject: 'lunch' })
    expect(router.url).toBe('/mail/m1/t9?subject=lunch&unread=1&flagged=1')
    await view.setFilter({ unread: false, flagged: false, subject: '' })
    expect(router.url).toBe('/mail/m1/t9')
    await view.closeThread()
    expect(router.url).toBe('/mail/m1')
  })

  it('opens a thread with the filter kept', async () => {
    await router.navigateByUrl('/mail/m2?flagged=1')
    await view.openThread('t3')
    expect(router.url).toBe('/mail/m2/t3?flagged=1')
    expect(view.filterQuery()).toEqual({ flagged: '1' })
  })

  it('is somewhere else on the settings pages', async () => {
    await router.navigateByUrl('/settings/security')
    expect(view.onMail()).toBe(false)
    expect(view.mailboxId()).toBe('')
  })

  it('sends someone who is not logged in to the login, with where they wanted to go', async () => {
    auth.account = null
    await router.navigateByUrl('/mail/m1/t9?unread=1')
    // A slash in the query stays a slash.
    expect(router.url).toBe('/login?next=/mail/m1/t9%3Funread%3D1')
    await router.navigateByUrl('/mail')
    expect(router.url).toBe('/login')
  })

  it('sends someone who is logged in away from the login', async () => {
    await router.navigateByUrl('/login?next=/settings')
    expect(router.url).toBe('/settings')
  })
})

describe('safeNext', () => {
  it('is a path of this app, never another site or the login itself', () => {
    expect(safeNext('/mail/m1')).toBe('/mail/m1')
    expect(safeNext('/settings/security')).toBe('/settings/security')
    expect(safeNext('//evil.example/x')).toBe('/mail')
    expect(safeNext('https://evil.example')).toBe('/mail')
    expect(safeNext('/login?next=/mail')).toBe('/mail')
    expect(safeNext(null)).toBe('/mail')
  })
})
