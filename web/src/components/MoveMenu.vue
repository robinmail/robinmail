<script setup lang="ts">
// A menu of mailboxes: "move to" picks one; "labels" ticks the mailboxes
// the messages are in.
import { computed } from 'vue'
import type { DropdownMenuItem } from '@nuxt/ui'
import type { Id } from '../jmap/types'
import { mailboxIcon } from '../lib/format'
import { useMailView } from '../lib/mailView'

const props = defineProps<{
  mode: 'move' | 'labels'
  // move: the mailbox that is left out. labels: not used.
  exclude?: Id
  // labels: the mailboxes all of the messages are in.
  checked?: Id[]
}>()
const emit = defineEmits<{ move: [to: Id]; label: [mailbox: Id, on: boolean] }>()

const { mailboxes } = useMailView()

const items = computed<DropdownMenuItem[][]>(() => [
  [{ label: props.mode === 'move' ? 'Move to' : 'Mailboxes of this conversation', type: 'label' }],
  mailboxes.value
    .filter((o) => props.mode === 'labels' || o.mailbox.id !== props.exclude)
    .map((o): DropdownMenuItem => {
      const base = { label: o.mailbox.name, icon: mailboxIcon(o.mailbox.role), class: o.depth ? 'ps-6' : '' }
      if (props.mode === 'move') return { ...base, onSelect: () => emit('move', o.mailbox.id) }
      return {
        ...base,
        type: 'checkbox',
        checked: props.checked?.includes(o.mailbox.id) ?? false,
        onUpdateChecked: (on: boolean) => emit('label', o.mailbox.id, on),
        onSelect: (e: Event) => e.preventDefault(), // stay open: several can be ticked
      }
    }),
])
</script>

<template>
  <UDropdownMenu :items="items" :content="{ align: 'end' }" :ui="{ content: 'max-h-80 overflow-y-auto min-w-48' }">
    <slot />
  </UDropdownMenu>
</template>
