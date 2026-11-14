// The objects every screen shares: who is logged in, the JMAP client and
// the mail store. Nothing here is kept in localStorage: the session is the
// cookie, which scripts cannot read.

import { reactive } from 'vue'
import type { Account } from './auth/api'
import { JmapClient } from './jmap/client'
import { ComposeStore } from './store/compose'
import { MailStore } from './store/mail'

export const auth = reactive({
  // Whether /auth/session was asked yet.
  checked: false,
  account: null as Account | null,
  // A login in this browser gave its password and still owes its code.
  waiting: false,
})

export const client = new JmapClient()
export const store = new MailStore(client)
export const compose = new ComposeStore(client, store)

// loadAccount fetches what every screen behind the login needs: the
// mailboxes, then the identities. Composers left from before a new login
// are saved by it.
export async function loadAccount(): Promise<void> {
  if (!store.state.ready) await store.load()
  await compose.init()
}
