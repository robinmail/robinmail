// The parts of JMAP core (RFC 8620) and JMAP mail (RFC 8621) the webmail
// uses. Property lists follow what robinmail serves; see PLAN.md, phase 3.

export type Id = string

export interface Session {
  username: string
  apiUrl: string
  downloadUrl: string
  uploadUrl: string
  eventSourceUrl: string
  state: string
  capabilities: Record<string, unknown>
  accounts: Record<string, { name: string; isPersonal: boolean; isReadOnly: boolean; accountCapabilities: Record<string, unknown> }>
  primaryAccounts: Record<string, Id>
}

export type Invocation = [name: string, args: Record<string, unknown>, callId: string]

export interface ResultReference {
  resultOf: string
  name: string
  path: string
}

export interface EmailAddress {
  name: string | null
  email: string
}

export interface Mailbox {
  id: Id
  name: string
  parentId: Id | null
  role: string | null
  sortOrder: number
  totalEmails: number
  unreadEmails: number
  totalThreads: number
  unreadThreads: number
  myRights: {
    mayReadItems: boolean
    mayAddItems: boolean
    mayRemoveItems: boolean
    maySetSeen: boolean
    maySetKeywords: boolean
    mayCreateChild: boolean
    mayRename: boolean
    mayDelete: boolean
    maySubmit: boolean
  }
  isSubscribed: boolean
}

export interface BodyPart {
  partId: string | null
  blobId: Id | null
  size: number
  name: string | null
  type: string
  charset?: string | null
  disposition: string | null
  cid: string | null
}

export interface BodyValue {
  value: string
  isEncodingProblem: boolean
  isTruncated: boolean
}

// An Email as the lists hold it. The body properties are there once the
// message was opened.
export interface Email {
  id: Id
  blobId: Id
  threadId: Id
  mailboxIds: Record<Id, true>
  keywords: Record<string, true>
  size: number
  receivedAt: string
  hasAttachment: boolean
  preview: string
  from: EmailAddress[] | null
  to: EmailAddress[] | null
  subject: string | null
  cc?: EmailAddress[] | null
  bcc?: EmailAddress[] | null
  replyTo?: EmailAddress[] | null
  sentAt?: string | null
  textBody?: BodyPart[]
  htmlBody?: BodyPart[]
  attachments?: BodyPart[]
  bodyValues?: Record<string, BodyValue>
}

export interface Thread {
  id: Id
  emailIds: Id[]
}

export type FilterCondition = {
  inMailbox?: Id
  inMailboxOtherThan?: Id[]
  hasKeyword?: string
  notKeyword?: string
  hasAttachment?: boolean
  from?: string
  to?: string
  subject?: string
}
export type Filter = FilterCondition | { operator: 'AND' | 'OR' | 'NOT'; conditions: Filter[] }

export interface Comparator {
  property: string
  isAscending?: boolean
  keyword?: string
}

export interface SetError {
  type: string
  description?: string | null
  properties?: string[]
}

// Arguments and responses per method. "#name" arguments (result
// references) are accepted for every argument by the Batch type.

export interface GetArgs {
  ids?: Id[] | null
  properties?: string[] | null
}
export interface GetResponse<T> {
  accountId: Id
  state: string
  list: T[]
  notFound: Id[]
}
export interface ChangesArgs {
  sinceState: string
  maxChanges?: number
}
export interface ChangesResponse {
  accountId: Id
  oldState: string
  newState: string
  hasMoreChanges: boolean
  created: Id[]
  updated: Id[]
  destroyed: Id[]
}
export interface SetArgs<T> {
  ifInState?: string | null
  create?: Record<string, Partial<T>>
  update?: Record<Id, Record<string, unknown>>
  destroy?: Id[]
}
export interface SetResponse<T> {
  accountId: Id
  oldState: string | null
  newState: string
  created: Record<string, Partial<T> & { id: Id }> | null
  updated: Record<Id, Partial<T> | null> | null
  destroyed: Id[] | null
  notCreated: Record<string, SetError> | null
  notUpdated: Record<Id, SetError> | null
  notDestroyed: Record<Id, SetError> | null
}
export interface QueryResponse {
  accountId: Id
  queryState: string
  canCalculateChanges: boolean
  position: number
  ids: Id[]
  total?: number
}
export interface QueryChangesResponse {
  accountId: Id
  oldQueryState: string
  newQueryState: string
  total?: number
  removed: Id[]
  added: { id: Id; index: number }[]
}
export interface EmailQueryArgs {
  filter?: Filter | null
  sort?: Comparator[] | null
  position?: number
  limit?: number
  collapseThreads?: boolean
  calculateTotal?: boolean
}
export interface EmailGetArgs extends GetArgs {
  bodyProperties?: string[]
  fetchTextBodyValues?: boolean
  fetchHTMLBodyValues?: boolean
  fetchAllBodyValues?: boolean
  maxBodyValueBytes?: number
}

export interface Methods {
  'Core/echo': { args: Record<string, unknown>; response: Record<string, unknown> }
  'Mailbox/get': { args: GetArgs; response: GetResponse<Mailbox> }
  'Mailbox/changes': { args: ChangesArgs; response: ChangesResponse & { updatedProperties: string[] | null } }
  'Mailbox/set': { args: SetArgs<Mailbox> & { onDestroyRemoveEmails?: boolean }; response: SetResponse<Mailbox> }
  'Thread/get': { args: GetArgs; response: GetResponse<Thread> }
  'Thread/changes': { args: ChangesArgs; response: ChangesResponse }
  'Email/get': { args: EmailGetArgs; response: GetResponse<Email> }
  'Email/changes': { args: ChangesArgs; response: ChangesResponse }
  'Email/query': { args: EmailQueryArgs; response: QueryResponse }
  'Email/queryChanges': {
    args: { filter?: Filter | null; sort?: Comparator[] | null; sinceQueryState: string; maxChanges?: number; collapseThreads?: boolean; calculateTotal?: boolean }
    response: QueryChangesResponse
  }
  'Email/set': { args: SetArgs<Email>; response: SetResponse<Email> }
}
export type MethodName = keyof Methods
