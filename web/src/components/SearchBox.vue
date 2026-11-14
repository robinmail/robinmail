<script setup lang="ts">
// The search box over the list: full-text search in the open mailbox
// (Email/query with the text condition). Words must all be in a message;
// "quoted words" must be there as written.
import { ref, watch } from 'vue'
import { useMailView } from '../lib/mailView'

const { spec, mailbox, setFilter } = useMailView()
const text = ref(spec.value.text)
const input = ref<{ inputRef?: HTMLInputElement | null } | null>(null)

watch(() => spec.value.text, (t) => {
  if (t !== text.value.trim()) text.value = t
})

// Typing searches a moment after the last key; Enter at once.
let timer: ReturnType<typeof setTimeout> | undefined
function typed() {
  clearTimeout(timer)
  timer = setTimeout(apply, 400)
}
function apply() {
  clearTimeout(timer)
  const t = text.value.trim()
  if (t !== spec.value.text) void setFilter({ text: t })
}
function clear() {
  text.value = ''
  apply()
}

defineExpose({ focus: () => input.value?.inputRef?.focus() })
</script>

<template>
  <form role="search" class="border-b border-default px-3 py-2" @submit.prevent="apply">
    <UInput
      ref="input" v-model="text" name="q" type="search" icon="i-lucide-search" autocomplete="off" class="w-full" data-testid="search"
      :placeholder="`Search ${mailbox?.name ?? 'mail'}`" :aria-label="`Search ${mailbox?.name ?? 'mail'}`"
      :ui="{ trailing: 'pe-1' }" @input="typed" @keydown.esc="clear"
    >
      <template v-if="text" #trailing>
        <UButton icon="i-lucide-x" color="neutral" variant="link" size="sm" aria-label="Clear the search" data-testid="search-clear" @click="clear" />
      </template>
    </UInput>
  </form>
</template>
