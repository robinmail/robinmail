<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { compose, store } from '../app'
import { shortDate, fullDate } from '../lib/format'
import { actions, selection, useMailView, type Row } from '../lib/mailView'
import type { ListSpec } from '../store/mail'
import { snippetParts } from '../lib/snippet'
import SearchBox from './SearchBox.vue'

const view = useMailView()
const { rows, threadId, mailboxId, mailbox, spec, filterCount, selectedRows } = view
const list = computed(() => store.state.list)
const isTrash = computed(() => mailbox.value?.role === 'trash')

function toggle(row: Row, on: boolean) {
  const next = new Set(selection.value)
  if (on) next.add(row.id)
  else next.delete(row.id)
  selection.value = next
}
const allSelected = computed(() => rows.value.length > 0 && selectedRows.value.length === rows.value.length)
function toggleAll(on: boolean) {
  selection.value = new Set(on ? rows.value.map((r) => r.id) : [])
}
// Rows that left the list are not selected any more.
watch(rows, (now) => {
  if (selection.value.size === 0) return
  const have = new Set(now.map((r) => r.id))
  const kept = [...selection.value].filter((id) => have.has(id))
  if (kept.length !== selection.value.size) selection.value = new Set(kept)
})

// An action on the selection. When the open thread is part of it and
// leaves the list, the view returns to the list.
async function bulk(run: (rows: Row[]) => Promise<boolean>, leaves: boolean) {
  const picked = selectedRows.value
  if (picked.length === 0) return
  if (leaves && picked.some((r) => r.threadId === threadId.value)) void view.closeThread()
  selection.value = new Set()
  await run(picked)
}

// Infinite scroll: the next page loads when the end of the list shows.
const sentinel = ref<HTMLElement | null>(null)
let observer: IntersectionObserver | undefined
onMounted(() => {
  observer = new IntersectionObserver((entries) => {
    if (entries.some((e) => e.isIntersecting)) void store.loadMore()
  }, { rootMargin: '200px' })
  if (sentinel.value) observer.observe(sentinel.value)
})
onBeforeUnmount(() => observer?.disconnect())
watch(sentinel, (el, old) => {
  if (old) observer?.unobserve(old)
  if (el) observer?.observe(el)
})

// In Drafts a row that is one draft opens in the composer, not as a thread.
function openRow(e: MouseEvent, row: Row) {
  if (mailbox.value?.role !== 'drafts' || row.emails.length !== 1 || !row.email.keywords.$draft) return
  if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return
  e.preventDefault()
  compose.openDraft(row.id).catch((err: unknown) => store.onError(err instanceof Error ? err.message : String(err)))
}

// What a search found in a row: the subject and a piece of the body with
// the matches marked. null where nothing matched, or without a search.
function snippet(id: string, field: 'subject' | 'preview') {
  const html = list.value.snippets[id]?.[field]
  return html ? snippetParts(html) : null
}

const search = ref<InstanceType<typeof SearchBox> | null>(null)
defineExpose({ focusSearch: () => search.value?.focus() })

const chips = computed(() => {
  const s = spec.value
  const out: { label: string; clear: Partial<ListSpec> }[] = []
  if (s.text) out.push({ label: `Search: ${s.text}`, clear: { text: '' } })
  if (s.from) out.push({ label: `From: ${s.from}`, clear: { from: '' } })
  if (s.subject) out.push({ label: `Subject: ${s.subject}`, clear: { subject: '' } })
  if (s.unread) out.push({ label: 'Unread', clear: { unread: false } })
  if (s.flagged) out.push({ label: 'Flagged', clear: { flagged: false } })
  if (s.attachment) out.push({ label: 'Has attachment', clear: { attachment: false } })
  return out
})
</script>

<template>
  <div class="flex min-h-0 flex-1 flex-col">
    <SearchBox ref="search" />
    <!-- Selection bar, or the active filters -->
    <div v-if="rows.length > 0 || filterCount > 0" class="flex min-h-11 items-center gap-1 border-b border-default px-3 py-1.5">
      <UCheckbox
        v-if="rows.length > 0" :model-value="allSelected ? true : selectedRows.length > 0 ? 'indeterminate' : false"
        aria-label="Select all" class="mr-2" @update:model-value="toggleAll($event === true)"
      />
      <template v-if="selectedRows.length > 0">
        <span class="mr-auto text-sm text-muted" data-testid="selected-count">{{ selectedRows.length }} selected</span>
        <UTooltip :text="selectedRows.some((r) => r.unread) ? 'Mark read' : 'Mark unread'">
          <UButton
            :icon="selectedRows.some((r) => r.unread) ? 'i-lucide-mail-open' : 'i-lucide-mail'" color="neutral" variant="ghost" size="sm"
            :aria-label="selectedRows.some((r) => r.unread) ? 'Mark read' : 'Mark unread'" @click="bulk((r) => actions.toggleRead(r), false)"
          />
        </UTooltip>
        <UTooltip text="Flag">
          <UButton icon="i-lucide-star" color="neutral" variant="ghost" size="sm" aria-label="Flag" @click="bulk((r) => actions.toggleFlag(r), false)" />
        </UTooltip>
        <MoveMenu mode="move" :exclude="mailboxId" @move="(to) => bulk((r) => actions.move(r, mailboxId, to), true)">
          <UButton icon="i-lucide-folder-input" color="neutral" variant="ghost" size="sm" aria-label="Move to" />
        </MoveMenu>
        <UTooltip :text="isTrash ? 'Delete forever' : 'Delete'">
          <UButton
            icon="i-lucide-trash-2" color="neutral" variant="ghost" size="sm" :aria-label="isTrash ? 'Delete forever' : 'Delete'"
            @click="bulk((r) => actions.remove(r), true)"
          />
        </UTooltip>
      </template>
      <div v-else class="flex flex-wrap items-center gap-1.5">
        <UBadge v-for="c in chips" :key="c.label" color="neutral" variant="subtle" class="max-w-48 gap-1">
          <span class="truncate">{{ c.label }}</span>
          <button type="button" class="rounded hover:text-highlighted" :aria-label="`Remove filter ${c.label}`" @click="view.setFilter(c.clear)">
            <UIcon name="i-lucide-x" class="size-3 align-middle" />
          </button>
        </UBadge>
        <span v-if="chips.length === 0" class="text-xs text-dimmed">{{ list.total }} {{ list.total === 1 ? 'conversation' : 'conversations' }}</span>
      </div>
    </div>

    <div class="min-h-0 flex-1 overflow-y-auto overscroll-contain" data-testid="message-list">
      <div v-if="list.loading && rows.length === 0" class="flex flex-col gap-4 p-4" aria-busy="true">
        <div v-for="n in 6" :key="n" class="flex flex-col gap-2">
          <USkeleton class="h-4 w-1/3" />
          <USkeleton class="h-4 w-2/3" />
          <USkeleton class="h-3 w-full" />
        </div>
      </div>

      <div v-else-if="list.error" class="flex flex-col items-center gap-3 p-8 text-center">
        <UIcon name="i-lucide-triangle-alert" class="size-8 text-error" />
        <p class="text-sm text-muted">
          The list could not be loaded.<br>{{ list.error }}
        </p>
        <UButton label="Try again" color="neutral" variant="subtle" @click="store.openList(spec)" />
      </div>

      <div v-else-if="rows.length === 0" class="flex flex-col items-center gap-3 p-8 pt-16 text-center">
        <UIcon :name="filterCount ? 'i-lucide-search-x' : 'i-lucide-inbox'" class="size-10 text-dimmed" />
        <p class="text-sm text-muted">
          {{ spec.text ? 'No messages match the search.' : filterCount ? 'No messages match the filter.' : 'No messages here.' }}
        </p>
        <UButton
          v-if="filterCount" :label="spec.text ? 'Clear the search' : 'Clear the filter'" color="neutral" variant="subtle" size="sm"
          @click="view.setFilter({ text: '', from: '', subject: '', unread: false, flagged: false, attachment: false })"
        />
      </div>

      <ul v-else role="list">
        <li
          v-for="row in rows" :key="row.id" :data-row="row.id" :data-thread="row.threadId" :data-unread="row.unread || undefined"
          class="group relative flex border-b border-default border-l-2"
          :class="[
            row.threadId === threadId ? 'border-l-primary bg-primary/10' : 'border-l-transparent hover:bg-elevated/50',
            selection.has(row.id) && row.threadId !== threadId ? 'bg-elevated' : '',
          ]"
        >
          <div class="flex flex-col items-center gap-2 py-3 pl-2.5 pr-1">
            <UCheckbox :model-value="selection.has(row.id)" :aria-label="`Select ${row.email.subject || 'message'}`" @update:model-value="toggle(row, $event === true)" />
            <button
              type="button" class="rounded p-0.5 focus-visible:outline-2 focus-visible:outline-primary"
              :class="row.flagged ? 'text-warning' : 'text-dimmed lg:opacity-0 lg:group-hover:opacity-100 focus-visible:opacity-100 hover:text-highlighted'"
              :aria-label="row.flagged ? 'Remove flag' : 'Flag'" :aria-pressed="row.flagged" data-testid="row-flag"
              @click="actions.toggleFlag([row])"
            >
              <UIcon name="i-lucide-star" class="block size-4" :class="row.flagged ? '[&>path]:fill-current' : ''" />
            </button>
          </div>
          <RouterLink
            :to="view.threadLocation(row.threadId)" :replace="!!threadId"
            class="min-w-0 flex-1 py-2.5 pl-1.5 pr-3 focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-primary"
            :aria-current="row.threadId === threadId ? 'true' : undefined" @click.capture="openRow($event, row)"
          >
            <div class="flex items-baseline gap-2">
              <span class="truncate text-sm" :class="row.unread ? 'font-semibold text-highlighted' : 'text-toned'">{{ row.names }}</span>
              <span v-if="row.emails.length > 1" class="shrink-0 text-xs tabular-nums text-dimmed" :aria-label="`${row.emails.length} messages`">{{ row.emails.length }}</span>
              <time
                class="ml-auto shrink-0 text-xs tabular-nums" :class="row.unread ? 'font-medium text-primary' : 'text-dimmed'"
                :datetime="row.email.receivedAt" :title="fullDate(row.email.receivedAt)"
              >{{ shortDate(row.email.receivedAt) }}</time>
            </div>
            <div class="flex items-center gap-1.5">
              <span v-if="row.unread" class="size-2 shrink-0 rounded-full bg-primary" role="img" aria-label="Unread" />
              <span class="truncate text-sm" :class="row.unread ? 'font-semibold text-highlighted' : 'text-default'">
                <template v-if="snippet(row.id, 'subject')">
                  <template v-for="(part, n) in snippet(row.id, 'subject')" :key="n">
                    <mark v-if="part.mark" class="rounded-sm bg-warning/30 text-inherit">{{ part.text }}</mark><template v-else>{{ part.text }}</template>
                  </template>
                </template>
                <template v-else>{{ row.email.subject || '(no subject)' }}</template>
              </span>
              <UIcon v-if="row.email.keywords.$answered" name="i-lucide-reply" class="size-3.5 shrink-0 text-dimmed" role="img" aria-label="Answered" />
              <UIcon v-if="row.attachment" name="i-lucide-paperclip" class="ml-auto size-3.5 shrink-0 text-dimmed" role="img" aria-label="Has attachment" />
            </div>
            <p v-if="snippet(row.id, 'preview')" class="truncate text-sm text-dimmed" data-testid="snippet">
              <template v-for="(part, n) in snippet(row.id, 'preview')" :key="n">
                <mark v-if="part.mark" class="rounded-sm bg-warning/30 text-default">{{ part.text }}</mark><template v-else>{{ part.text }}</template>
              </template>
            </p>
            <p v-else class="truncate text-sm text-dimmed">
              {{ row.email.preview || ' ' }}
            </p>
          </RouterLink>
        </li>
      </ul>

      <div v-if="rows.length > 0" ref="sentinel" class="flex justify-center p-4">
        <UButton
          v-if="list.ids.length < list.total" :loading="list.loadingMore" label="Load more" color="neutral" variant="ghost" size="sm"
          @click="store.loadMore()"
        />
        <span v-else-if="list.total > 10" class="text-xs text-dimmed">End of the list</span>
      </div>
    </div>
  </div>
</template>
