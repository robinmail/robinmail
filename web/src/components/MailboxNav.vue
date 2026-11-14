<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'
import type { DropdownMenuItem } from '@nuxt/ui'
import { store } from '../app'
import type { Id, Mailbox } from '../jmap/types'
import { mailboxIcon } from '../lib/format'
import { useMailView } from '../lib/mailView'

const router = useRouter()
const { mailboxes, mailboxId } = useMailView()

type Dialog = { kind: 'create'; parentId: Id | null } | { kind: 'rename'; mailbox: Mailbox } | { kind: 'delete'; mailbox: Mailbox }
const dialog = ref<Dialog | null>(null)
const open = computed({ get: () => dialog.value !== null, set: (v) => { if (!v) dialog.value = null } })
const name = ref('')
const parent = ref('none')
const busy = ref(false)
const error = ref('')

const parents = computed(() => [
  { label: 'None (top level)', value: 'none' },
  ...mailboxes.value.map((o) => ({ label: '  '.repeat(o.depth) + o.mailbox.name, value: o.mailbox.id })),
])

function show(d: Dialog) {
  dialog.value = d
  error.value = ''
  name.value = d.kind === 'rename' ? d.mailbox.name : ''
  parent.value = d.kind === 'create' && d.parentId ? d.parentId : 'none'
}

function menu(m: Mailbox): DropdownMenuItem[][] {
  return [
    [
      { label: 'Rename', icon: 'i-lucide-pencil', onSelect: () => show({ kind: 'rename', mailbox: m }) },
      { label: 'New mailbox inside', icon: 'i-lucide-folder-plus', onSelect: () => show({ kind: 'create', parentId: m.id }) },
    ],
    [{ label: 'Delete', icon: 'i-lucide-trash-2', color: 'error', onSelect: () => show({ kind: 'delete', mailbox: m }) }],
  ]
}

const title = computed(() => {
  const d = dialog.value
  if (!d) return ''
  if (d.kind === 'create') return 'New mailbox'
  return d.kind === 'rename' ? `Rename “${d.mailbox.name}”` : `Delete “${d.mailbox.name}”?`
})

async function submit() {
  const d = dialog.value
  if (!d || busy.value) return
  const n = name.value.trim()
  if (d.kind !== 'delete' && !n) {
    error.value = 'Give the mailbox a name.'
    return
  }
  busy.value = true
  error.value = ''
  try {
    if (d.kind === 'create') {
      const id = await store.createMailbox(n, parent.value === 'none' ? null : parent.value)
      if (id) void router.push({ name: 'mail', params: { mailbox: id } })
    } else if (d.kind === 'rename') {
      if (n !== d.mailbox.name) await store.renameMailbox(d.mailbox.id, n)
    } else {
      await store.destroyMailbox(d.mailbox.id, d.mailbox.totalEmails > 0)
    }
    dialog.value = null
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <nav class="flex flex-col gap-0.5" aria-label="Mailboxes">
    <div v-for="o in mailboxes" :key="o.mailbox.id" class="group flex items-center rounded-md" :class="o.mailbox.id === mailboxId ? 'bg-elevated' : 'hover:bg-elevated/50'">
      <RouterLink
        :to="{ name: 'mail', params: { mailbox: o.mailbox.id } }"
        class="flex min-w-0 flex-1 items-center gap-2 py-1.5 pr-2 text-sm rounded-md focus-visible:outline-2 focus-visible:outline-primary"
        :class="o.mailbox.id === mailboxId ? 'text-highlighted font-medium' : 'text-muted group-hover:text-highlighted'"
        :style="{ paddingLeft: `${0.625 + o.depth * 1.125}rem` }"
        :aria-current="o.mailbox.id === mailboxId ? 'page' : undefined"
        :data-mailbox="o.mailbox.role || o.mailbox.name"
      >
        <UIcon :name="mailboxIcon(o.mailbox.role)" class="size-4 shrink-0" />
        <span class="truncate flex-1">{{ o.mailbox.name }}</span>
        <span
          v-if="o.mailbox.unreadEmails > 0" class="text-xs font-semibold tabular-nums text-primary" data-unread
          :aria-label="`${o.mailbox.unreadEmails} unread`"
        >{{ o.mailbox.unreadEmails }}</span>
        <span v-else-if="o.mailbox.role === 'trash' && o.mailbox.totalEmails > 0" class="text-xs tabular-nums text-dimmed" data-total>{{ o.mailbox.totalEmails }}</span>
      </RouterLink>
      <UDropdownMenu v-if="!o.mailbox.role" :items="menu(o.mailbox)" :content="{ align: 'start' }">
        <UButton
          icon="i-lucide-ellipsis" size="xs" color="neutral" variant="ghost" :aria-label="`Actions for ${o.mailbox.name}`"
          class="mr-1 lg:opacity-0 lg:group-hover:opacity-100 focus-visible:opacity-100 data-[state=open]:opacity-100"
        />
      </UDropdownMenu>
    </div>
    <UButton
      icon="i-lucide-plus" label="New mailbox" color="neutral" variant="ghost" size="sm" class="mt-2 justify-start text-muted"
      @click="show({ kind: 'create', parentId: null })"
    />

    <UModal v-model:open="open" :title="title" :description="dialog?.kind === 'delete' ? 'This cannot be undone.' : 'Mailboxes work as labels: a message can be in several.'">
      <template #body>
        <form id="mailbox-form" class="flex flex-col gap-4" @submit.prevent="submit">
          <UAlert v-if="error" color="error" variant="subtle" icon="i-lucide-circle-alert" :title="error" role="alert" />
          <template v-if="dialog?.kind === 'delete'">
            <p v-if="dialog.mailbox.totalEmails > 0" class="text-sm">
              Its {{ dialog.mailbox.totalEmails }} {{ dialog.mailbox.totalEmails === 1 ? 'message is' : 'messages are' }} deleted with it.
              A message that is also in another mailbox stays there.
            </p>
            <p v-else class="text-sm">
              The mailbox is empty.
            </p>
          </template>
          <template v-else>
            <UFormField label="Name" name="name">
              <UInput v-model="name" name="name" autofocus maxlength="255" class="w-full" autocomplete="off" />
            </UFormField>
            <UFormField v-if="dialog?.kind === 'create'" label="Inside" name="parent">
              <USelect v-model="parent" :items="parents" class="w-full" />
            </UFormField>
          </template>
        </form>
      </template>
      <template #footer>
        <div class="flex w-full justify-end gap-2">
          <UButton label="Cancel" color="neutral" variant="ghost" @click="dialog = null" />
          <UButton
            type="submit" form="mailbox-form" :loading="busy" :color="dialog?.kind === 'delete' ? 'error' : 'primary'"
            :label="dialog?.kind === 'delete' ? 'Delete' : dialog?.kind === 'rename' ? 'Rename' : 'Create'"
          />
        </div>
      </template>
    </UModal>
  </nav>
</template>
