// The objects every screen shares: who is logged in, the JMAP client and
// the mail store. Nothing here is kept in localStorage: the session is the
// cookie, which scripts cannot read.

import { reactive } from 'vue'
import { JmapClient, type Account } from './jmap/client'
import { MailStore } from './store/mail'

export const auth = reactive({
  // Whether /auth/session was asked yet.
  checked: false,
  account: null as Account | null,
})

export const client = new JmapClient()
export const store = new MailStore(client)
