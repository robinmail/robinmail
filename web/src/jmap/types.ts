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
  messageId?: string[] | null
  inReplyTo?: string[] | null
  references?: string[] | null
  textBody?: BodyPart[]
  htmlBody?: BodyPart[]
  attachments?: BodyPart[]
  bodyValues?: Record<string, BodyValue>
}

// What Email/set takes for a new Email (RFC 8621 4.6), as far as the
// composer uses it. A body part has a partId (its text is in bodyValues)
// or a blobId (an upload, or a part of another message).
export interface NewBodyPart {
  partId?: string
  blobId?: Id
  type: string
  name?: string | null
  disposition?: string | null
  cid?: string | null
}
export interface EmailCreate {
  mailboxIds: Record<Id, true>
  keywords: Record<string, true>
  from: EmailAddress[]
  to?: EmailAddress[]
  cc?: EmailAddress[]
  bcc?: EmailAddress[]
  replyTo?: EmailAddress[]
  subject: string
  inReplyTo?: string[]
  references?: string[]
  textBody: NewBodyPart[]
  htmlBody?: NewBodyPart[]
  attachments?: NewBodyPart[]
  bodyValues: Record<string, { value: string }>
  [header: `header:${string}`]: string
}

export interface Identity {
  id: Id
  name: string
  email: string
  replyTo: EmailAddress[] | null
  bcc: EmailAddress[] | null
  textSignature: string
  htmlSignature: string
  mayDelete: boolean
}

export interface DeliveryStatus {
  smtpReply: string
  delivered: 'queued' | 'yes' | 'no' | 'unknown'
  displayed: 'unknown' | 'yes'
}

export interface EmailSubmission {
  id: Id
  identityId: Id
  emailId: Id
  threadId: Id
  sendAt: string
  undoStatus: 'pending' | 'final' | 'canceled'
  deliveryStatus: Record<string, DeliveryStatus> | null
}

// SearchSnippet (RFC 8621 5): the subject and a part of the body as HTML
// in which only <mark> and </mark> are markup; null where nothing matched.
export interface SearchSnippet {
  emailId: Id
  subject: string | null
  preview: string | null
}

// VacationResponse (RFC 8621 8): the automatic reply of the account. There
// is one, with the id "singleton". Dates are UTC; null is no bound.
export interface VacationResponse {
  id: Id
  isEnabled: boolean
  fromDate: string | null
  toDate: string | null
  subject: string | null
  textBody: string | null
  htmlBody: string | null
}

// SieveScript (RFC 9661 2): a script of the account. The script itself is
// the blob; at most one script is active.
export interface SieveScript {
  id: Id
  name: string | null
  blobId: Id
  isActive: boolean
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
  // Full-text search (RFC 8621 4.4.1): words that must all be in the
  // message, "quoted text" as a phrase.
  text?: string
  body?: string
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
  // EmailSubmission/set (RFC 8621 7.5).
  invalidRecipients?: string[]
  maxRecipients?: number
  maxSize?: number
  notFound?: Id[]
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
  'SearchSnippet/get': {
    args: { filter?: Filter | null; emailIds?: Id[] }
    response: { accountId: Id; list: SearchSnippet[]; notFound: Id[] | null }
  }
  'Email/set': {
    args: Omit<SetArgs<Email>, 'create'> & { create?: Record<string, EmailCreate> }
    response: SetResponse<Email>
  }
  'VacationResponse/get': { args: GetArgs; response: GetResponse<VacationResponse> }
  'VacationResponse/set': { args: SetArgs<VacationResponse>; response: SetResponse<VacationResponse> }
  'SieveScript/get': { args: GetArgs; response: GetResponse<SieveScript> }
  'SieveScript/set': {
    args: SetArgs<SieveScript> & { onSuccessActivateScript?: Id | null; onSuccessDeactivateScript?: boolean | null }
    response: SetResponse<SieveScript>
  }
  'SieveScript/validate': { args: { blobId: Id }; response: { accountId: Id; error: SetError | null } }
  'Identity/get': { args: GetArgs; response: GetResponse<Identity> }
  'Identity/set': { args: SetArgs<Identity>; response: SetResponse<Identity> }
  'EmailSubmission/get': { args: GetArgs; response: GetResponse<EmailSubmission> }
  'EmailSubmission/changes': { args: ChangesArgs; response: ChangesResponse }
  'EmailSubmission/query': {
    args: { filter?: { emailIds?: Id[]; threadIds?: Id[]; undoStatus?: string } | null; sort?: Comparator[] | null; limit?: number }
    response: QueryResponse
  }
  'EmailSubmission/set': {
    args: Omit<SetArgs<EmailSubmission>, 'create'> & {
      create?: Record<string, { identityId: Id; emailId: Id }>
      onSuccessUpdateEmail?: Record<string, Record<string, unknown>> | null
      onSuccessDestroyEmail?: string[] | null
    }
    response: SetResponse<EmailSubmission>
  }
}
export type MethodName = keyof Methods
