import { type EnvironmentProviders, inject, makeEnvironmentProviders, provideEnvironmentInitializer } from '@angular/core'
import { DefaultUrlSerializer, Router, type Routes, type UrlMatcher, UrlSegment, UrlSerializer, type UrlTree } from '@angular/router'
import { Frame } from './frame'
import { LoginPage } from './pages/login-page'
import { sessionGuard, watchSession } from './session'
import { ColorMode } from './ui/color-mode'

// /mail, /mail/<mailbox id> and /mail/<mailbox id>/<thread id> are one
// route, so that going from one to the other keeps the page as it is.
const mail: UrlMatcher = (segments) => {
  if (segments[0]?.path !== 'mail' || segments.length > 3) return null
  const posParams: Record<string, UrlSegment> = {}
  if (segments[1]) posParams['mailbox'] = segments[1]
  if (segments[2]) posParams['thread'] = segments[2]
  return { consumed: segments, posParams }
}

// The routes of the webmail, for the router of the host application. The
// login comes with the first load; the pages behind it are fetched when
// they are first opened.
export const webmailRoutes: Routes = [
  {
    path: '',
    component: Frame,
    canActivateChild: [sessionGuard],
    children: [
      { path: 'login', component: LoginPage, data: { name: 'login' } },
      { matcher: mail, loadComponent: () => import('./pages/mail-page').then((m) => m.MailPage), data: { name: 'mail' } },
      { path: 'settings', loadComponent: () => import('./pages/settings-page').then((m) => m.SettingsPage), data: { name: 'settings' } },
      { path: 'settings/filters', loadComponent: () => import('./pages/filters-page').then((m) => m.FiltersPage), data: { name: 'filters' } },
      { path: 'settings/vacation', loadComponent: () => import('./pages/vacation-page').then((m) => m.VacationPage), data: { name: 'vacation' } },
      { path: 'settings/security', loadComponent: () => import('./pages/security-page').then((m) => m.SecurityPage), data: { name: 'security' } },
      { path: '**', redirectTo: 'mail' },
    ],
  },
]

// Angular writes a slash in a query value as %2F. It need not be: the
// address after a login reads /login?next=/mail/m1, as it does in the other
// webmail.
class PlainSlashes extends DefaultUrlSerializer {
  override serialize(tree: UrlTree): string {
    const url = super.serialize(tree)
    const q = url.indexOf('?')
    return q === -1 ? url : url.slice(0, q) + url.slice(q).replace(/%2F/gi, '/')
  }
}

// provideWebmail sets up what the screens need besides their routes.
export function provideWebmail(): EnvironmentProviders {
  return makeEnvironmentProviders([
    { provide: UrlSerializer, useClass: PlainSlashes },
    provideEnvironmentInitializer(() => {
      watchSession(inject(Router))
      inject(ColorMode)
    }),
  ])
}
