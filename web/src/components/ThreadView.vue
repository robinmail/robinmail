<script setup lang="ts">
// The messages of one thread. Opening a thread marks it read.
import { computed, ref, watch } from 'vue'
import type { DropdownMenuItem } from '@nuxt/ui'
import { client, compose, store } from '../app'
import type { Kind } from '../compose/message'
import type { DeliveryStatus, Email, Id } from '../jmap/types'
import { addressList, displayName, fullDate, shortDate } from '../lib/format'
import { actions, buildRow, useMailView } from '../lib/mailView'

const view = useMailView()
const { threadId, mailboxId, mailbox } = view

const emails = computed(() => store.threadEmails(threadId.value))
const loading = ref(false)
const error = ref('')
// Which messages are open. The rest show one line.
const expanded = ref(new Set<Id>())

// The thread as a row: what the toolbar acts on. Built from the newest
// email in this mailbox, also when the list does not show the thread.
const row = computed(() => {
  if (view.currentRow.value) return view.currentRow.value
  const inBox = emails.value.filter((e) => e.mailboxIds[mailboxId.value])
  const newest = (inBox.length ? inBox : emails.value).at(-1)
  return newest ? buildRow(newest.id, mailboxId.value, false) : null
})
const subject = computed(() => emails.value[0]?.subject || '(no subject)')
const isTrash = computed(() => mailbox.value?.role === 'trash')

async function load(id: Id) {
  if (!id) return
  error.value = ''
  loading.value = store.threadEmails(id).length === 0
  try {
    await store.openThread(id)
    if (id !== threadId.value) return
    const list = store.threadEmails(id)
    if (list.length === 0) {
      error.value = 'This conversation no longer exists.'
      return
    }
    // Open what is unread, and always the last message.
    const unread = list.filter((e) => !e.keywords.$seen)
    expanded.value = new Set([...unread.map((e) => e.id), list.at(-1)!.id])
    if (unread.length) void store.setKeyword(unread.map((e) => e.id), '$seen', true)
  } catch (e) {
    if (id === threadId.value) error.value = e instanceof Error ? e.message : String(e)
  } finally {
    if (id === threadId.value) loading.value = false
  }
}
watch(threadId, load, { immediate: true })

// A message that arrived in the open thread has no body yet.
watch(() => emails.value.filter((e) => e.bodyValues === undefined).map((e) => e.id), (missing) => {
  if (missing.length && !loading.value) {
    void store.loadBodies(missing)
    expanded.value = new Set([...expanded.value, ...missing])
  }
})
// The thread emptied under us: deleted here or somewhere else.
watch(() => emails.value.length, (n, old) => {
  if (n === 0 && old > 0) void view.closeThread()
})

function toggle(id: Id) {
  const next = new Set(expanded.value)
  if (next.has(id)) next.delete(id)
  else next.add(id)
  expanded.value = next
}

// Thread actions.
function markUnread() {
  if (!row.value) return
  void actions.setRead([row.value], false)
  void view.closeThread() // or opening it would mark it read again
}
function flag() {
  if (row.value) void actions.toggleFlag([row.value])
}
function move(to: Id) {
  const r = row.value
  if (!r) return
  void view.leave()
  void actions.move([r], mailboxId.value, to)
}
function remove() {
  const r = row.value
  if (!r) return
  void view.leave()
  void actions.remove([r])
}

// Reply, reply all, forward: about one message, or without one the last
// message of the thread that is not a draft. It answers whether a
// composer opened: a message whose body is not loaded yet cannot be quoted.
function respond(kind: Exclude<Kind, 'new'>, email?: Email): boolean {
  const e = email ?? emails.value.filter((x) => !x.keywords.$draft).at(-1)
  if (!e || e.bodyValues === undefined || !compose.state.identitiesLoaded) return false
  compose.respond(kind, e)
  return true
}

async function editDraft(e: Email) {
  try {
    await compose.openDraft(e.id)
  } catch (err) {
    store.onError(err instanceof Error ? err.message : String(err))
  }
}
defineExpose({ markUnread, flag, remove, respond })

// What became of a message we sent, per recipient. The submissions of
// the messages in Sent are asked for once and then follow the changes.
const sentId = computed(() => store.mailboxByRole('sent')?.id)
watch(() => emails.value.filter((e) => sentId.value && e.mailboxIds[sentId.value]).map((e) => e.id), (ids) => {
  if (ids.length) void compose.loadSubmissions(ids)
}, { immediate: true })

interface Delivery {
  address: string
  label: string
  detail: string
  color: 'success' | 'neutral' | 'warning' | 'error'
  icon: string
}
function delivery(address: string, d: DeliveryStatus, pending: boolean): Delivery {
  // "unknown" is what JMAP calls mail another server took over: sent, as far as anyone can know.
  if (d.delivered === 'yes') return { address, label: 'Delivered', detail: d.smtpReply, color: 'success', icon: 'i-lucide-check-check' }
  if (d.delivered === 'unknown') return { address, label: 'Sent', detail: d.smtpReply || 'The receiving server accepted it.', color: 'neutral', icon: 'i-lucide-check' }
  if (d.delivered === 'no') return { address, label: 'Failed', detail: d.smtpReply, color: 'error', icon: 'i-lucide-circle-x' }
  return pending
    ? { address, label: 'Sending', detail: 'It leaves in a few seconds.', color: 'neutral', icon: 'i-lucide-clock' }
    : { address, label: 'Queued', detail: d.smtpReply || 'Waiting for its turn.', color: d.smtpReply ? 'warning' : 'neutral', icon: 'i-lucide-clock' }
}
function deliveries(e: Email): { canceled: boolean; list: Delivery[] } | null {
  const s = compose.state.submissions[e.id]
  if (!s) return null
  if (s.undoStatus === 'canceled') return { canceled: true, list: [] }
  return { canceled: false, list: Object.entries(s.deliveryStatus ?? {}).map(([a, d]) => delivery(a, d, s.undoStatus === 'pending')) }
}

// Labels: a mailbox is ticked when every message of the thread is in it.
const checked = computed(() =>
  Object.keys(store.state.mailboxes).filter((id) => emails.value.length > 0 && emails.value.every((e) => e.mailboxIds[id])),
)
function label(id: Id, on: boolean) {
  const leaves = !on && id === mailboxId.value
  if (leaves) void view.leave()
  void store.label(emails.value.map((e) => e.id), id, on)
}

function labels(e: Email) {
  return Object.keys(e.mailboxIds).map((id) => store.state.mailboxes[id]).filter((m) => !!m)
}

function menu(e: Email): DropdownMenuItem[][] {
  const trash = store.mailboxByRole('trash')
  const inTrash = !!trash && !!e.mailboxIds[trash.id]
  const last = emails.value.length === 1
  return [
    ...(e.keywords.$draft
      ? [[{ label: 'Edit this draft', icon: 'i-lucide-pen-line', onSelect: () => void editDraft(e) }]]
      : [[
          { label: 'Reply', icon: 'i-lucide-reply', kbds: ['r'], onSelect: () => void respond('reply', e) },
          { label: 'Reply all', icon: 'i-lucide-reply-all', kbds: ['a'], onSelect: () => void respond('replyAll', e) },
          { label: 'Forward', icon: 'i-lucide-forward', kbds: ['f'], onSelect: () => void respond('forward', e) },
          { label: 'Forward as attachment', icon: 'i-lucide-paperclip', onSelect: () => void respond('forwardAttached', e) },
        ]]),
    [
      e.keywords.$flagged
        ? { label: 'Remove flag', icon: 'i-lucide-star-off', onSelect: () => void store.setKeyword([e.id], '$flagged', false) }
        : { label: 'Flag', icon: 'i-lucide-star', onSelect: () => void store.setKeyword([e.id], '$flagged', true) },
      { label: 'Mark unread from here', icon: 'i-lucide-mail', onSelect: () => {
        const from = emails.value.findIndex((x) => x.id === e.id)
        void store.setKeyword(emails.value.slice(from).map((x) => x.id), '$seen', false)
        void view.closeThread()
      } },
      { label: 'Download original', icon: 'i-lucide-download', onSelect: () => {
        window.location.assign(client.downloadUrl(e.blobId, `${(e.subject || 'message').slice(0, 80)}.eml`, 'message/rfc822'))
      } },
    ],
    [
      { label: inTrash ? 'Delete this message forever' : 'Delete this message', icon: 'i-lucide-trash-2', color: 'error', onSelect: () => {
        if (last) void view.leave()
        void store.remove([e.id])
      } },
    ],
  ]
}
</script>

<template>
  <div class="flex min-h-0 flex-1 flex-col" data-testid="thread">
    <div class="flex h-(--ui-header-height) shrink-0 items-center gap-1 border-b border-default px-2 sm:px-4">
      <UTooltip text="Back to the list" :kbds="['Esc']">
        <UButton icon="i-lucide-arrow-left" color="neutral" variant="ghost" aria-label="Back to the list" class="lg:hidden" @click="view.closeThread()" />
      </UTooltip>
      <UTooltip text="Close" :kbds="['Esc']">
        <UButton icon="i-lucide-x" color="neutral" variant="ghost" aria-label="Close the conversation" class="hidden lg:inline-flex" @click="view.closeThread()" />
      </UTooltip>
      <div class="ml-auto flex items-center gap-0.5" role="toolbar" aria-label="Conversation actions">
        <UTooltip text="Reply" :kbds="['R']">
          <UButton icon="i-lucide-reply" color="neutral" variant="ghost" aria-label="Reply" :disabled="!row" class="max-sm:hidden" @click="respond('reply')" />
        </UTooltip>
        <UTooltip text="Mark unread" :kbds="['U']">
          <UButton icon="i-lucide-mail" color="neutral" variant="ghost" aria-label="Mark unread" :disabled="!row" @click="markUnread" />
        </UTooltip>
        <UTooltip :text="row?.flagged ? 'Remove flag' : 'Flag'" :kbds="['S']">
          <UButton
            icon="i-lucide-star" color="neutral" variant="ghost" :aria-label="row?.flagged ? 'Remove flag' : 'Flag'" :aria-pressed="row?.flagged ?? false"
            :disabled="!row" :class="row?.flagged ? 'text-warning [&_path]:fill-current' : ''" data-testid="thread-flag" @click="flag"
          />
        </UTooltip>
        <MoveMenu mode="labels" :checked="checked" @label="label">
          <UButton icon="i-lucide-tags" color="neutral" variant="ghost" aria-label="Mailboxes of this conversation" :disabled="!row" data-testid="thread-labels" />
        </MoveMenu>
        <MoveMenu mode="move" :exclude="mailboxId" @move="move">
          <UButton icon="i-lucide-folder-input" color="neutral" variant="ghost" aria-label="Move to" :disabled="!row" data-testid="thread-move" />
        </MoveMenu>
        <UTooltip :text="isTrash ? 'Delete forever' : 'Delete'" :kbds="['#']">
          <UButton
            icon="i-lucide-trash-2" color="neutral" variant="ghost" :aria-label="isTrash ? 'Delete forever' : 'Delete'" :disabled="!row"
            data-testid="thread-delete" @click="remove"
          />
        </UTooltip>
        <USeparator orientation="vertical" class="mx-1 h-5" />
        <UTooltip text="Newer" :kbds="['K']">
          <UButton icon="i-lucide-chevron-up" color="neutral" variant="ghost" aria-label="Newer conversation" @click="view.step(-1)" />
        </UTooltip>
        <UTooltip text="Older" :kbds="['J']">
          <UButton icon="i-lucide-chevron-down" color="neutral" variant="ghost" aria-label="Older conversation" @click="view.step(1)" />
        </UTooltip>
      </div>
    </div>

    <div class="min-h-0 flex-1 overflow-y-auto overscroll-contain">
      <div v-if="error" class="flex flex-col items-center gap-3 p-8 pt-16 text-center">
        <UIcon name="i-lucide-triangle-alert" class="size-8 text-error" />
        <p class="text-sm text-muted">
          {{ error }}
        </p>
        <UButton label="Back to the list" color="neutral" variant="subtle" @click="view.closeThread()" />
      </div>
      <div v-else-if="loading" class="flex flex-col gap-3 p-6" aria-busy="true">
        <USkeleton class="h-7 w-2/3" />
        <USkeleton class="h-4 w-1/3" />
        <USkeleton class="mt-4 h-40 w-full" />
      </div>
      <div v-else class="mx-auto flex max-w-4xl flex-col gap-3 p-3 sm:p-6">
        <h1 class="px-1 text-xl font-semibold text-highlighted break-words" data-testid="thread-subject">
          {{ subject }}
        </h1>
        <article
          v-for="e in emails" :key="e.id" :data-email="e.id" class="rounded-lg border border-default bg-default"
          :aria-label="`Message from ${addressList(e.from) || 'unknown'}`"
        >
          <div class="flex items-start gap-2 px-3 py-2.5 sm:px-4">
            <button
              type="button" class="flex min-w-0 flex-1 flex-col text-left rounded focus-visible:outline-2 focus-visible:outline-primary"
              :aria-expanded="expanded.has(e.id)" @click="toggle(e.id)"
            >
              <span class="flex w-full items-baseline gap-2">
                <span class="truncate text-sm font-semibold text-highlighted">{{ (e.from ?? []).map(displayName).join(', ') || '(unknown sender)' }}</span>
                <UBadge v-if="e.keywords.$draft" label="Draft" color="warning" variant="subtle" size="sm" class="shrink-0 self-center" />
                <UIcon v-if="e.keywords.$answered" name="i-lucide-reply" class="size-3.5 shrink-0 self-center text-dimmed" role="img" aria-label="Answered" data-testid="answered" />
                <UIcon v-if="e.keywords.$forwarded" name="i-lucide-forward" class="size-3.5 shrink-0 self-center text-dimmed" role="img" aria-label="Forwarded" />
                <UIcon v-if="e.keywords.$flagged" name="i-lucide-star" class="size-3.5 shrink-0 self-center text-warning [&>path]:fill-current" role="img" aria-label="Flagged" />
                <time class="ml-auto shrink-0 text-xs text-dimmed" :datetime="e.receivedAt" :title="fullDate(e.sentAt || e.receivedAt)">{{ shortDate(e.receivedAt) }}</time>
              </span>
              <span v-if="expanded.has(e.id)" class="w-full truncate text-xs text-muted">
                {{ (e.from ?? []).map((a) => a.email).join(', ') }} to {{ (e.to ?? []).map(displayName).join(', ') || 'nobody' }}<template v-if="e.cc?.length">, cc {{ e.cc.map(displayName).join(', ') }}</template>
              </span>
              <span v-else class="w-full truncate text-sm text-dimmed">{{ e.preview }}</span>
            </button>
            <UDropdownMenu :items="menu(e)" :content="{ align: 'end' }">
              <UButton icon="i-lucide-ellipsis-vertical" color="neutral" variant="ghost" size="sm" aria-label="Message actions" />
            </UDropdownMenu>
          </div>
          <div v-if="expanded.has(e.id)" class="flex flex-col gap-3 px-3 pb-3 sm:px-4 sm:pb-4">
            <div v-if="labels(e).length > 1 || !e.mailboxIds[mailboxId]" class="flex flex-wrap gap-1.5" aria-label="Mailboxes">
              <UBadge v-for="m in labels(e)" :key="m.id" color="neutral" variant="subtle" size="sm" :label="m.name" />
            </div>
            <template v-if="deliveries(e)">
              <p v-if="deliveries(e)!.canceled" class="text-xs text-dimmed" data-testid="delivery">
                This message was taken back before it was sent.
              </p>
              <ul v-else class="flex flex-col gap-1" aria-label="Delivery" data-testid="delivery">
                <li v-for="d in deliveries(e)!.list" :key="d.address" class="flex flex-wrap items-baseline gap-x-2 text-xs" :data-delivery="d.label">
                  <UBadge :label="d.label" :icon="d.icon" :color="d.color" variant="subtle" size="sm" class="self-center" />
                  <span class="text-muted">{{ d.address }}</span>
                  <span v-if="d.detail" class="min-w-0 break-words" :class="d.color === 'error' ? 'text-error' : 'text-dimmed'">{{ d.detail }}</span>
                </li>
              </ul>
            </template>
            <MessageBody v-if="e.bodyValues !== undefined" :email="e" />
            <USkeleton v-else class="h-24 w-full" />
            <div v-if="e.keywords.$draft">
              <UButton label="Edit this draft" icon="i-lucide-pen-line" size="sm" color="neutral" variant="subtle" data-testid="edit-draft" @click="editDraft(e)" />
            </div>
          </div>
        </article>
        <div v-if="emails.some((e) => !e.keywords.$draft)" class="flex flex-wrap gap-2 px-1 pb-2">
          <UButton label="Reply" icon="i-lucide-reply" color="neutral" variant="outline" data-testid="reply" @click="respond('reply')" />
          <UButton label="Reply all" icon="i-lucide-reply-all" color="neutral" variant="outline" data-testid="reply-all" @click="respond('replyAll')" />
          <UButton label="Forward" icon="i-lucide-forward" color="neutral" variant="outline" data-testid="forward" @click="respond('forward')" />
        </div>
      </div>
    </div>
  </div>
</template>
