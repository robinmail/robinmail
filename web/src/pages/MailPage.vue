<script setup lang="ts">
// The three panes: mailboxes, the message list, the open thread. On a
// narrow screen the mailboxes are a drawer and the thread replaces the list.
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { auth, store } from '../app'
import { logout } from '../jmap/client'
import { actions, selection, useMailView } from '../lib/mailView'
import ThreadView from '../components/ThreadView.vue'

const router = useRouter()
const toast = useToast()
const view = useMailView()
const { mailboxId, threadId, mailbox, spec } = view

const loadError = ref('')
const filterOpen = ref(false)
const thread = ref<InstanceType<typeof ThreadView> | null>(null)

store.onError = (message) => toast.add({ title: message, color: 'error', icon: 'i-lucide-circle-alert' })

async function load() {
  loadError.value = ''
  try {
    await store.load()
  } catch (e) {
    if (auth.account) loadError.value = e instanceof Error ? e.message : String(e)
  }
}

// The route decides what is shown. Without a mailbox, or with one that
// does not exist (any more), it is the inbox.
watch(
  () => [store.state.ready, spec.value] as const,
  ([ready, s]) => {
    if (!ready) return
    if (!store.state.mailboxes[s.mailboxId]) {
      const inbox = store.mailboxByRole('inbox') ?? Object.values(store.state.mailboxes)[0]
      if (inbox) void router.replace({ name: 'mail', params: { mailbox: inbox.id } })
      return
    }
    void store.openList(s)
  },
  { immediate: true, deep: true },
)
watch(() => store.state.list.key, () => (selection.value = new Set()))
watch(() => store.state.mailboxes[mailboxId.value], (m) => {
  // The open mailbox was deleted.
  if (store.state.ready && !m && mailboxId.value) void router.replace({ name: 'mail' })
})

// Push does not exist yet (phase 6): ask for changes every half minute
// while the page is visible, and when the user comes back to it.
const POLL_MS = 30_000
let timer: ReturnType<typeof setInterval> | undefined
let last = 0
function refresh(force = false) {
  if (document.visibilityState !== 'visible') return
  const now = Date.now()
  if (!force && now - last < 2000) return
  last = now
  void store.refresh()
}
const onFocus = () => refresh()
const onVisible = () => refresh()

// Keyboard: j/k next and previous, u read/unread, s flag, # or Delete
// delete, Escape back to the list, / the filter.
function onKey(e: KeyboardEvent) {
  if (e.defaultPrevented || e.metaKey || e.ctrlKey || e.altKey) return
  const t = e.target as HTMLElement | null
  if (t && (t.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(t.tagName))) return
  if (document.querySelector('[role="dialog"], [role="menu"]')) return
  const rows = view.selectedRows.value.length ? view.selectedRows.value : view.currentRow.value ? [view.currentRow.value] : []
  const open = rows.length === 1 && rows[0]!.threadId === threadId.value && view.selectedRows.value.length === 0
  switch (e.key) {
    case 'j':
      view.step(1)
      break
    case 'k':
      view.step(-1)
      break
    case 'u':
      if (open) thread.value?.markUnread()
      else if (rows.length) void actions.toggleRead(rows)
      break
    case 's':
      if (rows.length) void actions.toggleFlag(rows)
      break
    case '#':
    case 'Delete':
      if (open) thread.value?.remove()
      else if (rows.length) {
        selection.value = new Set()
        void actions.remove(rows)
      }
      break
    case 'Escape':
      if (view.selectedRows.value.length) selection.value = new Set()
      else if (threadId.value) void view.closeThread()
      break
    case '/':
      filterOpen.value = true
      break
    default:
      return
  }
  e.preventDefault()
}

onMounted(() => {
  if (!store.state.ready) void load()
  timer = setInterval(() => refresh(true), POLL_MS)
  window.addEventListener('focus', onFocus)
  window.addEventListener('online', onFocus)
  document.addEventListener('visibilitychange', onVisible)
  window.addEventListener('keydown', onKey)
})
onBeforeUnmount(() => {
  clearInterval(timer)
  window.removeEventListener('focus', onFocus)
  window.removeEventListener('online', onFocus)
  document.removeEventListener('visibilitychange', onVisible)
  window.removeEventListener('keydown', onKey)
})

async function signOut() {
  try {
    await logout()
  } finally {
    auth.account = null
    store.reset()
    await router.replace({ name: 'login' })
  }
}

// The title of the tab: unread in the inbox, and where we are.
const title = computed(() => {
  const unread = store.mailboxByRole('inbox')?.unreadEmails ?? 0
  return `${unread ? `(${unread}) ` : ''}${mailbox.value?.name ?? 'Mail'} · robinmail`
})
watch(title, (t) => (document.title = t), { immediate: true })

const shortcuts = [
  ['J', 'Next conversation'], ['K', 'Previous conversation'], ['U', 'Mark read or unread'], ['S', 'Flag'],
  ['#', 'Delete'], ['/', 'Filter'], ['Esc', 'Back to the list'],
] as const
</script>

<template>
  <div v-if="loadError" class="flex min-h-full flex-col items-center justify-center gap-4 p-6 text-center">
    <UIcon name="i-lucide-cloud-off" class="size-10 text-dimmed" />
    <p class="text-muted">
      The mail could not be loaded.<br><span class="text-sm">{{ loadError }}</span>
    </p>
    <UButton label="Try again" @click="load" />
  </div>

  <UDashboardGroup v-else storage="local" storage-key="robinmail">
    <UDashboardSidebar id="mailboxes" resizable :min-size="12" :default-size="16" :max-size="24" :ui="{ footer: 'border-t border-default' }">
      <template #header>
        <RouterLink :to="{ name: 'mail' }" class="flex items-center gap-2 rounded focus-visible:outline-2 focus-visible:outline-primary">
          <img src="/favicon.svg" alt="" class="size-6">
          <span class="font-semibold tracking-tight text-highlighted">robinmail</span>
        </RouterLink>
      </template>

      <MailboxNav v-if="store.state.ready" />
      <div v-else class="flex flex-col gap-2 px-1" aria-busy="true">
        <USkeleton v-for="n in 5" :key="n" class="h-7 w-full" />
      </div>

      <template #footer>
        <div class="flex w-full min-w-0 items-center gap-1">
          <span class="min-w-0 flex-1 truncate text-sm text-muted" :title="store.state.username || auth.account?.username">{{ store.state.username || auth.account?.username }}</span>
          <UPopover :content="{ side: 'top', align: 'end' }">
            <UButton icon="i-lucide-keyboard" color="neutral" variant="ghost" size="sm" aria-label="Keyboard shortcuts" class="hidden lg:inline-flex" />
            <template #content>
              <dl class="grid grid-cols-[auto_1fr] items-center gap-x-3 gap-y-1.5 p-4 text-sm">
                <template v-for="[key, what] in shortcuts" :key="key">
                  <dt><UKbd :value="key" /></dt>
                  <dd class="text-muted">
                    {{ what }}
                  </dd>
                </template>
              </dl>
            </template>
          </UPopover>
          <UColorModeButton color="neutral" variant="ghost" size="sm" />
          <UTooltip text="Log out">
            <UButton icon="i-lucide-log-out" color="neutral" variant="ghost" size="sm" aria-label="Log out" data-testid="logout" @click="signOut" />
          </UTooltip>
        </div>
      </template>
    </UDashboardSidebar>

    <UDashboardPanel
      id="list" resizable :min-size="22" :default-size="30" :max-size="45"
      :class="threadId ? 'hidden lg:flex' : ''" :ui="{ root: 'max-lg:w-full' }"
    >
      <UDashboardNavbar :title="mailbox?.name ?? 'Mail'" :icon="undefined">
        <template #trailing>
          <UBadge v-if="mailbox && mailbox.unreadEmails > 0" :label="mailbox.unreadEmails" variant="subtle" size="sm" :aria-label="`${mailbox.unreadEmails} unread`" />
        </template>
        <template #right>
          <UTooltip v-if="store.state.offline" text="The server does not answer. Trying again.">
            <UIcon name="i-lucide-cloud-off" class="size-4 text-warning" role="img" aria-label="Offline" />
          </UTooltip>
          <UTooltip text="Check for new mail">
            <UButton icon="i-lucide-refresh-cw" color="neutral" variant="ghost" aria-label="Check for new mail" @click="refresh(true)" />
          </UTooltip>
          <FilterBox v-model:open="filterOpen" />
        </template>
      </UDashboardNavbar>
      <MessageList />
    </UDashboardPanel>

    <UDashboardPanel id="thread" :class="threadId ? '' : 'hidden lg:flex'">
      <ThreadView v-if="threadId && store.state.ready" ref="thread" />
      <div v-else-if="!threadId" class="flex flex-1 flex-col items-center justify-center gap-3 text-dimmed">
        <UIcon name="i-lucide-mail-open" class="size-16" />
        <p class="text-sm">
          Choose a conversation to read it.
        </p>
      </div>
    </UDashboardPanel>
  </UDashboardGroup>
</template>
