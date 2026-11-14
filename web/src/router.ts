import { createRouter, createWebHistory, type RouteLocationNormalized } from 'vue-router'
import { auth, client, store } from './app'
import { currentSession } from './auth/api'
import LoginPage from './pages/LoginPage.vue'

// The route carries what a reload or the back button must bring back:
// /mail/<mailbox id>/<thread id>?from=&subject=&unread=1&flagged=1&attachment=1
export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/login', name: 'login', component: LoginPage },
    { path: '/mail/:mailbox?/:thread?', name: 'mail', component: () => import('./pages/MailPage.vue') },
    { path: '/settings', name: 'settings', component: () => import('./pages/SettingsPage.vue') },
    { path: '/settings/security', name: 'security', component: () => import('./pages/SecurityPage.vue') },
    { path: '/:rest(.*)*', redirect: { name: 'mail' } },
  ],
})

// safeNext is where to go after login: a path of this app, never another site.
export function safeNext(next: unknown): string {
  return typeof next === 'string' && next.startsWith('/') && !next.startsWith('//') && !next.startsWith('/login') ? next : '/mail'
}

router.beforeEach(async (to: RouteLocationNormalized) => {
  if (!auth.checked) {
    try {
      const { session, waiting } = await currentSession()
      auth.account = session
      auth.waiting = waiting
    } catch {
      auth.account = null // the login page shows what is wrong when the user tries
    }
    auth.checked = true
  }
  if (to.name !== 'login' && !auth.account) {
    return { name: 'login', query: to.fullPath === '/mail' || to.fullPath === '/' ? {} : { next: to.fullPath } }
  }
  if (to.name === 'login' && auth.account) return safeNext(to.query.next)
  return true
})

// A 401 from the API: the session ended. Forget what we hold and log in again.
client.onUnauthorized = () => {
  if (!auth.account) return
  auth.account = null
  store.reset()
  const here = router.currentRoute.value
  // The composers stay as they are: what was typed is saved after the login.
  void router.replace({ name: 'login', query: here.name === 'mail' || here.name === 'settings' || here.name === 'security' ? { next: here.fullPath } : {} })
}
