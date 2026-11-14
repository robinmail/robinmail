<script setup lang="ts">
// One message being written: the fields, the body as plain or rich text,
// attachments, and the buttons to send, close or discard it.
import { computed, nextTick, onMounted, reactive, ref, watch } from 'vue'
import { client, compose } from '../app'
import { formatAddress } from '../compose/address'
import type { Attachment } from '../compose/message'
import { fileSize } from '../lib/format'
import type { Composer } from '../store/compose'
import AddressInput from './AddressInput.vue'
import RichEditor from './RichEditor.vue'

const props = defineProps<{ composer: Composer; phone: boolean }>()
const toast = useToast()

const c = computed(() => props.composer)
const draft = computed(() => props.composer.draft)
const uid = `composer-${props.composer.key}`

const showCc = ref(draft.value.cc.length + draft.value.bcc.length > 0)
const invalid = reactive({ to: false, cc: false, bcc: false })
const toField = ref<InstanceType<typeof AddressInput> | null>(null)
const ccField = ref<InstanceType<typeof AddressInput> | null>(null)
const bccField = ref<InstanceType<typeof AddressInput> | null>(null)
const textarea = ref<HTMLTextAreaElement | null>(null)
const editor = ref<InstanceType<typeof RichEditor> | null>(null)
const fileInput = ref<HTMLInputElement | null>(null)
const dragging = ref(0)
const confirmDiscard = ref(false)
let noSubjectOk = false

const names: Record<Composer['kind'], string> = { new: 'New message', reply: 'Reply', replyAll: 'Reply', forward: 'Forward', forwardAttached: 'Forward' }
const title = computed(() => draft.value.subject.trim() || names[c.value.kind])
const status = computed(() => {
  if (c.value.sending) return 'Sending…'
  if (c.value.saving) return 'Saving…'
  if (c.value.saveError) return 'Not saved'
  if (c.value.savedId && c.value.version === c.value.savedVersion) return 'Saved'
  return ''
})

const identities = computed(() => compose.state.identities.map((i) => ({ label: formatAddress({ name: i.name, email: i.email }), value: i.id })))
const from = computed(() => compose.identity(draft.value.identityId))

function touch() {
  compose.touch(c.value)
}

function focusBody() {
  if (draft.value.rich) editor.value?.focus()
  else if (textarea.value) {
    textarea.value.focus()
    textarea.value.setSelectionRange(0, 0)
    textarea.value.scrollTop = 0
  }
}

onMounted(() => {
  if (props.phone && c.value.kind !== 'new') return
  void nextTick(() => {
    if (draft.value.to.length === 0) toField.value?.focus()
    else focusBody()
  })
})

// --- attachments ---

function imageUrl(cid: string): string | null {
  const a = draft.value.attachments.find((x) => x.cid === cid)
  return a?.blobId ? client.downloadUrl(a.blobId, a.name, a.type) : null
}

function addFiles(files: File[], inline: boolean) {
  if (files.length === 0) return
  const { refused } = compose.attach(c.value, files.map((file) => ({ file, name: file.name || 'file' })), inline)
  for (const r of refused) toast.add({ title: r, color: 'error', icon: 'i-lucide-circle-alert' })
}

// An inline image goes into the text once it is uploaded: the page shows
// it from the server.
const placed = new Set(draft.value.attachments.map((a) => a.key))
watch(() => draft.value.attachments.map((a) => `${a.key}:${a.state}`), () => {
  for (const a of draft.value.attachments) {
    if (!a.inline || a.state !== 'done' || placed.has(a.key)) continue
    placed.add(a.key)
    const url = imageUrl(a.cid)
    if (url && draft.value.rich) editor.value?.insertImage(a.cid, url)
  }
})

function removeAttachment(a: Attachment) {
  compose.removeAttachment(c.value, a)
  if (a.inline) editor.value?.removeImage(a.cid)
}

function pick(e: Event) {
  const el = e.target as HTMLInputElement
  addFiles([...(el.files ?? [])], false)
  el.value = ''
}

function onPastePlain(e: ClipboardEvent) {
  const files = [...(e.clipboardData?.files ?? [])]
  if (files.length === 0) return
  e.preventDefault()
  addFiles(files, false)
}

const hasFiles = (e: DragEvent) => [...(e.dataTransfer?.types ?? [])].includes('Files')
function onDrop(e: DragEvent) {
  dragging.value = 0
  if (!hasFiles(e)) return
  e.preventDefault()
  addFiles([...(e.dataTransfer?.files ?? [])], false)
}

// --- the body ---

function toggleRich() {
  compose.setRich(c.value, !draft.value.rich)
  try {
    localStorage.setItem('robinmail-rich', draft.value.rich ? '1' : '0')
  } catch {
    // Storage is off: the choice holds for this visit.
  }
  void nextTick(focusBody)
}

// --- send, close, discard ---

async function send() {
  if (c.value.sending) return
  const fields = [toField.value, ccField.value, bccField.value]
  if (!fields.map((f) => f?.commit() ?? true).every(Boolean)) {
    c.value.sendError = 'A recipient field holds something that is not an email address.'
    return
  }
  await nextTick()
  if (draft.value.subject.trim() === '' && !noSubjectOk && compose.problems(c.value).length === 0) {
    noSubjectOk = true
    c.value.sendError = 'This message has no subject. Send again to send it as it is.'
    return
  }
  // The dock shows the notice with "undo": this panel is gone by then.
  await compose.send(c.value)
}

async function close() {
  await compose.close(c.value)
}

function discard() {
  if (!confirmDiscard.value && (c.value.savedId || c.value.version > 0)) {
    confirmDiscard.value = true
    setTimeout(() => (confirmDiscard.value = false), 4000)
    return
  }
  void compose.discard(c.value)
}

function onKey(e: KeyboardEvent) {
  if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
    e.preventDefault()
    void send()
  }
}

const sendKeys = /Mac|iPhone|iPad/.test(navigator.platform) ? ['⌘', 'Enter'] : ['Ctrl', 'Enter']
</script>

<template>
  <section
    :aria-label="`Message: ${title}`" :data-composer="composer.key" data-testid="composer"
    class="pointer-events-auto relative flex flex-col border border-default bg-default shadow-2xl"
    :class="[
      composer.minimized
        ? 'w-full rounded-t-lg sm:w-64'
        : 'max-sm:fixed max-sm:inset-0 max-sm:border-0 sm:h-[min(40rem,calc(100dvh-4.5rem))] sm:w-[36rem] sm:rounded-t-lg',
    ]"
    @keydown="onKey" @dragenter="hasFiles($event) && dragging++" @dragleave="dragging = Math.max(0, dragging - 1)"
    @dragover="hasFiles($event) && $event.preventDefault()" @drop="onDrop"
  >
    <header class="flex h-10 shrink-0 items-center gap-1 rounded-t-lg bg-elevated pl-3 pr-1" :class="composer.minimized ? '' : 'max-sm:rounded-none'">
      <button
        type="button" class="flex min-w-0 flex-1 items-center gap-2 self-stretch text-left focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-primary"
        :aria-expanded="!composer.minimized" :aria-label="composer.minimized ? `Open ${title}` : `Minimise ${title}`"
        @click="composer.minimized = !composer.minimized"
      >
        <span class="truncate text-sm font-medium text-highlighted">{{ title }}</span>
        <span
          v-if="status" class="shrink-0 text-xs" :class="composer.saveError ? 'text-error' : 'text-dimmed'" data-testid="composer-status"
          aria-live="polite"
        >{{ status }}</span>
      </button>
      <UTooltip :text="composer.minimized ? 'Open' : 'Minimise'">
        <UButton
          :icon="composer.minimized ? 'i-lucide-chevron-up' : 'i-lucide-minus'" size="xs" color="neutral" variant="ghost"
          :aria-label="composer.minimized ? 'Open' : 'Minimise'" @click="composer.minimized = !composer.minimized"
        />
      </UTooltip>
      <UTooltip text="Save and close">
        <UButton icon="i-lucide-x" size="xs" color="neutral" variant="ghost" aria-label="Save and close" data-testid="composer-close" @click="close" />
      </UTooltip>
    </header>

    <div v-show="!composer.minimized" class="flex min-h-0 flex-1 flex-col" :inert="composer.sending" :class="composer.sending ? 'opacity-60' : ''">
      <UAlert
        v-if="composer.sendError || composer.saveError" color="error" variant="subtle" icon="i-lucide-circle-alert" role="alert"
        :title="composer.sendError || composer.saveError" class="m-2 mb-0" data-testid="composer-error"
        :ui="{ title: 'text-sm font-normal' }"
      />

      <div class="flex flex-col divide-y divide-default border-b border-default px-3 text-sm">
        <div v-if="identities.length > 1" class="flex items-center gap-2 py-1">
          <label :for="`${uid}-from`" class="w-12 shrink-0 text-dimmed">From</label>
          <USelect
            :id="`${uid}-from`" :model-value="draft.identityId" :items="identities" variant="none" size="sm" class="min-w-0 flex-1"
            @update:model-value="compose.setIdentity(composer, String($event))"
          />
        </div>
        <div class="flex items-start gap-2">
          <label :for="`${uid}-to`" class="w-12 shrink-0 py-1.5 text-dimmed">To</label>
          <AddressInput
            ref="toField" :model-value="draft.to" label="To" :input-id="`${uid}-to`"
            @update:model-value="draft.to = $event; touch()" @invalid="invalid.to = $event"
          />
          <UButton
            v-if="!showCc" label="Cc Bcc" size="xs" color="neutral" variant="ghost" class="mt-1 shrink-0" aria-label="Add Cc and Bcc"
            @click="showCc = true; nextTick(() => ccField?.focus())"
          />
        </div>
        <template v-if="showCc">
          <div class="flex items-start gap-2">
            <label :for="`${uid}-cc`" class="w-12 shrink-0 py-1.5 text-dimmed">Cc</label>
            <AddressInput
              ref="ccField" :model-value="draft.cc" label="Cc" :input-id="`${uid}-cc`"
              @update:model-value="draft.cc = $event; touch()" @invalid="invalid.cc = $event"
            />
          </div>
          <div class="flex items-start gap-2">
            <label :for="`${uid}-bcc`" class="w-12 shrink-0 py-1.5 text-dimmed">Bcc</label>
            <AddressInput
              ref="bccField" :model-value="draft.bcc" label="Bcc" :input-id="`${uid}-bcc`"
              @update:model-value="draft.bcc = $event; touch()" @invalid="invalid.bcc = $event"
            />
          </div>
        </template>
        <div class="flex items-center gap-2">
          <label :for="`${uid}-subject`" class="w-12 shrink-0 text-dimmed">Subject</label>
          <input
            :id="`${uid}-subject`" v-model="draft.subject" type="text" maxlength="998" autocomplete="off"
            class="min-w-0 flex-1 bg-transparent py-1.5 text-sm text-highlighted outline-none" @input="touch"
          >
        </div>
      </div>

      <RichEditor
        v-if="draft.rich" ref="editor" :html="draft.html" :image-url="imageUrl" label="Message"
        @update:html="draft.html = $event; touch()" @files="addFiles($event, true)"
      />
      <textarea
        v-else ref="textarea" v-model="draft.text" aria-label="Message" data-testid="plain-body"
        class="min-h-0 flex-1 resize-none bg-transparent px-3 py-2 font-sans text-sm leading-relaxed text-default outline-none"
        @input="touch" @paste="onPastePlain"
      />

      <ul v-if="draft.attachments.length > 0" class="flex max-h-28 flex-wrap gap-1.5 overflow-y-auto border-t border-default px-3 py-2" aria-label="Attachments">
        <li
          v-for="a in draft.attachments" :key="a.key" class="flex max-w-full items-center gap-1.5 rounded-md border py-1 pl-2 pr-1 text-xs"
          :class="a.state === 'failed' ? 'border-error/50 text-error' : 'border-default text-default'" data-testid="composer-attachment" :data-state="a.state"
        >
          <UIcon
            :name="a.state === 'uploading' ? 'i-lucide-loader-circle' : a.state === 'failed' ? 'i-lucide-circle-alert' : a.type.startsWith('image/') ? 'i-lucide-image' : 'i-lucide-paperclip'"
            class="size-3.5 shrink-0" :class="a.state === 'uploading' ? 'animate-spin text-dimmed' : a.state === 'failed' ? '' : 'text-dimmed'"
          />
          <span class="max-w-48 truncate" :title="a.name">{{ a.name }}</span>
          <span v-if="a.state === 'done'" class="shrink-0 text-dimmed">{{ fileSize(a.size) }}<template v-if="a.inline"> · in the text</template></span>
          <template v-else-if="a.state === 'uploading'">
            <UProgress :model-value="Math.round(a.progress * 100)" size="xs" class="w-16" :aria-label="`Uploading ${a.name}`" />
            <span class="shrink-0 tabular-nums text-dimmed">{{ Math.round(a.progress * 100) }}%</span>
          </template>
          <template v-else>
            <span class="max-w-56 truncate" :title="a.error">{{ a.error }}</span>
            <UButton label="Try again" size="xs" color="neutral" variant="link" @click="compose.retryUpload(composer, a)" />
          </template>
          <button
            type="button" class="shrink-0 rounded p-0.5 text-dimmed hover:bg-elevated hover:text-highlighted focus-visible:outline-2 focus-visible:outline-primary"
            :aria-label="a.state === 'uploading' ? `Cancel the upload of ${a.name}` : `Remove ${a.name}`" @click="removeAttachment(a)"
          >
            <UIcon name="i-lucide-x" class="block size-3.5" />
          </button>
        </li>
      </ul>

      <footer class="flex shrink-0 items-center gap-1 border-t border-default px-2 py-2 max-sm:pb-[max(0.5rem,env(safe-area-inset-bottom))]">
        <UTooltip text="Send" :kbds="sendKeys">
          <UButton label="Send" icon="i-lucide-send" :loading="composer.sending" data-testid="composer-send" @click="send" />
        </UTooltip>
        <input ref="fileInput" type="file" multiple class="hidden" data-testid="composer-file" @change="pick">
        <UTooltip text="Attach files">
          <UButton icon="i-lucide-paperclip" color="neutral" variant="ghost" aria-label="Attach files" @click="fileInput?.click()" />
        </UTooltip>
        <UTooltip :text="draft.rich ? 'Switch to plain text' : 'Switch to rich text'">
          <UButton
            icon="i-lucide-type" color="neutral" :variant="draft.rich ? 'soft' : 'ghost'" aria-label="Rich text" :aria-pressed="draft.rich"
            data-testid="composer-rich" @click="toggleRich"
          />
        </UTooltip>
        <span v-if="from && identities.length <= 1" class="ml-2 min-w-0 truncate text-xs text-dimmed" :title="from.email">from {{ from.email }}</span>
        <UTooltip :text="confirmDiscard ? 'Click again to discard this draft' : 'Discard'">
          <UButton
            icon="i-lucide-trash-2" :label="confirmDiscard ? 'Discard?' : undefined" :color="confirmDiscard ? 'error' : 'neutral'"
            :variant="confirmDiscard ? 'soft' : 'ghost'" aria-label="Discard this draft" class="ml-auto" data-testid="composer-discard" @click="discard"
          />
        </UTooltip>
      </footer>
    </div>

    <div
      v-if="dragging > 0 && !composer.minimized" class="pointer-events-none absolute inset-0 flex items-center justify-center rounded-t-lg border-2 border-dashed border-primary bg-default/90"
      aria-hidden="true"
    >
      <span class="flex items-center gap-2 text-sm font-medium text-primary"><UIcon name="i-lucide-paperclip" class="size-4" /> Drop files to attach them</span>
    </div>
  </section>
</template>
