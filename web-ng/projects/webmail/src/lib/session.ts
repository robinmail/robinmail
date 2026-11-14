// Who may see which route, and what happens when the session ends.

import { inject } from '@angular/core'
import { type CanActivateChildFn, Router } from '@angular/router'
import { authApi } from '@robinmail/jmap'
import { auth, client, store } from './app'

// safeNext is where to go after login: a path of this app, never another site.
export function safeNext(next: unknown): string {
  return typeof next === 'string' && next.startsWith('/') && !next.startsWith('//') && !next.startsWith('/login') ? next : '/mail'
}

export const sessionGuard: CanActivateChildFn = async (route, state) => {
  const router = inject(Router)
  if (!auth.checked) {
    try {
      const { session, waiting } = await authApi.currentSession()
      auth.account = session
      auth.waiting = waiting
    } catch {
      auth.account = null // the login page shows what is wrong when the user tries
    }
    auth.checked = true
  }
  const login = route.data['name'] === 'login'
  if (!login && !auth.account) {
    const plain = state.url === '/mail' || state.url === '/'
    return router.createUrlTree(['/login'], { queryParams: plain ? {} : { next: state.url } })
  }
  if (login && auth.account) return router.parseUrl(safeNext(route.queryParamMap.get('next')))
  return true
}

// A 401 from the API: the session ended. Forget what we hold and log in again.
export function watchSession(router: Router): void {
  client.onUnauthorized = () => {
    if (!auth.account) return
    auth.account = null
    store.reset()
    const here = router.url
    // The composers stay as they are: what was typed is saved after the login.
    void router.navigate(['/login'], { queryParams: here.startsWith('/mail') || here.startsWith('/settings') ? { next: here } : {}, replaceUrl: true })
  }
}
