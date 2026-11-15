// @robinmail/jmap: the JMAP client, the mail and compose stores, and the
// plain logic around them. Nothing here renders anything.

export * from './lib/reactive'
export * from './lib/jmap/types'
export * from './lib/jmap/client'
export * from './lib/jmap/sync'
export * from './lib/jmap/push'
export * from './lib/store/mail'
export * from './lib/store/compose'
export * from './lib/store/calendar'
export * from './lib/store/contacts'
export * from './lib/calendar/time'
export * from './lib/compose/address'
export * from './lib/compose/message'
export * from './lib/compose/text'
// The /auth API and the Sieve rules have names of their own (Session,
// quote, describe): they come as namespaces.
export * as authApi from './lib/auth/api'
export type { Account } from './lib/auth/api'
export * as sieveRules from './lib/sieve/rules'
