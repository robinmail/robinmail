<script setup lang="ts">
// A field of addresses: each one a chip, typed or pasted text read as a
// list. What is not an address stays in the field as text and is marked.
import { computed, ref } from 'vue'
import type { EmailAddress } from '../jmap/types'
import { dedupe, isValidEmail, parseAddresses } from '../compose/address'

const props = defineProps<{ modelValue: EmailAddress[]; label: string; inputId: string }>()
const emit = defineEmits<{ 'update:modelValue': [EmailAddress[]]; invalid: [boolean] }>()

const text = ref('')
const bad = ref(false)
const input = ref<HTMLInputElement | null>(null)
const describedBy = computed(() => (bad.value ? `${props.inputId}-error` : undefined))

// commit turns the typed text into chips. It answers whether all of it
// was an address.
function commit(): boolean {
  if (text.value.trim() === '') {
    text.value = ''
    setBad(false)
    return true
  }
  const r = parseAddresses(text.value)
  if (r.addresses.length) emit('update:modelValue', dedupe([...props.modelValue, ...r.addresses]))
  text.value = r.invalid.join(', ')
  setBad(r.invalid.length > 0)
  return r.invalid.length === 0
}

function setBad(v: boolean) {
  if (bad.value === v) return
  bad.value = v
  emit('invalid', v)
}

function remove(i: number) {
  emit('update:modelValue', props.modelValue.filter((_, at) => at !== i))
  input.value?.focus()
}

function onKey(e: KeyboardEvent) {
  if (e.key === 'Enter' || e.key === ',' || e.key === ';') {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) return // send
    e.preventDefault()
    commit()
  } else if (e.key === ' ' && isValidEmail(text.value.trim())) {
    e.preventDefault()
    commit()
  } else if (e.key === 'Tab' && text.value.trim() !== '') {
    if (!commit()) e.preventDefault()
  } else if (e.key === 'Backspace' && text.value === '' && props.modelValue.length > 0) {
    remove(props.modelValue.length - 1)
  }
}

function onPaste(e: ClipboardEvent) {
  const pasted = e.clipboardData?.getData('text/plain') ?? ''
  if (pasted === '') return
  e.preventDefault()
  text.value = text.value.trim() ? `${text.value}, ${pasted}` : pasted
  commit()
}

function onInput() {
  if (bad.value && text.value.trim() === '') setBad(false)
}

defineExpose({ focus: () => input.value?.focus(), commit })
</script>

<template>
  <div class="flex min-w-0 flex-1 flex-col">
    <div class="flex min-w-0 flex-wrap items-center gap-1 py-1" @click="input?.focus()">
      <span
        v-for="(a, i) in modelValue" :key="a.email" class="flex max-w-full items-center gap-1 rounded-full bg-elevated py-0.5 pl-2.5 pr-1 text-sm text-default"
        :title="a.email" data-testid="address-chip"
      >
        <span class="truncate">{{ a.name?.trim() || a.email }}</span>
        <button
          type="button" class="rounded-full p-0.5 text-dimmed hover:bg-accented hover:text-highlighted focus-visible:outline-2 focus-visible:outline-primary"
          :aria-label="`Remove ${a.email}`" @click.stop="remove(i)"
        >
          <UIcon name="i-lucide-x" class="block size-3.5" />
        </button>
      </span>
      <input
        :id="inputId" ref="input" v-model="text" type="text" inputmode="email" autocomplete="off" autocapitalize="off" spellcheck="false"
        :aria-label="label" :aria-invalid="bad || undefined" :aria-describedby="describedBy"
        class="min-w-32 flex-1 bg-transparent py-0.5 text-sm outline-none placeholder:text-dimmed" :class="bad ? 'text-error' : 'text-highlighted'"
        @keydown="onKey" @paste="onPaste" @blur="commit" @input="onInput"
      >
    </div>
    <p v-if="bad" :id="`${inputId}-error`" class="pb-1 text-xs text-error" role="alert">
      This is not an email address. Correct it or remove it.
    </p>
  </div>
</template>
