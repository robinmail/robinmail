<script setup lang="ts">
// The filter of the list. It holds only what Email/query supports on this
// server: from, subject, unread, flagged, has attachment (PLAN.md, phase 3).
import { computed, ref, watch } from 'vue'
import { useMailView } from '../lib/mailView'

const { spec, setFilter } = useMailView()
// The search text has its own box over the list and is not counted here.
const filterCount = computed(() => [spec.value.from, spec.value.subject, spec.value.unread, spec.value.flagged, spec.value.attachment].filter(Boolean).length)
const open = defineModel<boolean>('open', { default: false })
const from = ref(spec.value.from)
const subject = ref(spec.value.subject)

watch(() => [spec.value.from, spec.value.subject], ([f, s]) => {
  from.value = f ?? ''
  subject.value = s ?? ''
})

// Typing changes the route a moment after the last key.
let timer: ReturnType<typeof setTimeout> | undefined
function typed() {
  clearTimeout(timer)
  timer = setTimeout(apply, 350)
}
function apply() {
  clearTimeout(timer)
  const f = from.value.trim()
  const s = subject.value.trim()
  if (f !== spec.value.from || s !== spec.value.subject) void setFilter({ from: f, subject: s })
}
function clear() {
  clearTimeout(timer)
  void setFilter({ from: '', subject: '', unread: false, flagged: false, attachment: false })
}
</script>

<template>
  <UPopover v-model:open="open" :content="{ align: 'end' }">
    <UChip :show="filterCount > 0" :text="filterCount" size="2xl" inset>
      <UButton icon="i-lucide-list-filter" color="neutral" variant="ghost" aria-label="Filter messages" data-testid="filter" />
    </UChip>
    <template #content>
      <form class="flex w-72 max-w-[calc(100vw-2rem)] flex-col gap-3 p-4" @submit.prevent="apply(); open = false">
        <UFormField label="From">
          <UInput v-model="from" name="from" placeholder="Name or address" class="w-full" autocomplete="off" autofocus @input="typed" />
        </UFormField>
        <UFormField label="Subject">
          <UInput v-model="subject" name="subject" placeholder="Words in the subject" class="w-full" autocomplete="off" @input="typed" />
        </UFormField>
        <UCheckbox :model-value="spec.unread" label="Unread" @update:model-value="setFilter({ unread: $event === true })" />
        <UCheckbox :model-value="spec.flagged" label="Flagged" @update:model-value="setFilter({ flagged: $event === true })" />
        <UCheckbox :model-value="spec.attachment" label="Has attachment" @update:model-value="setFilter({ attachment: $event === true })" />
        <div class="flex justify-between gap-2 pt-1">
          <UButton label="Clear" color="neutral" variant="ghost" size="sm" :disabled="filterCount === 0" @click="clear" />
          <UButton type="submit" label="Done" size="sm" />
        </div>
      </form>
    </template>
  </UPopover>
</template>
