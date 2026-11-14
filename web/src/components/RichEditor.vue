<script setup lang="ts">
// A small rich text editor on contenteditable: bold, italic, lists,
// quotes, links and inline images. The browser does the editing
// (execCommand); what leaves the editor is written by exportHtml, which
// knows only those few elements. Pasted and dropped content comes in as
// plain text, so no foreign markup gets into the page.
import { onMounted, ref } from 'vue'
import { exportHtml, imageElement, importHtml, safeHref } from '../compose/richdom'

const props = defineProps<{ html: string; imageUrl: (cid: string) => string | null; label: string }>()
const emit = defineEmits<{ 'update:html': [string]; files: [File[]] }>()

const el = ref<HTMLDivElement | null>(null)
const active = ref({ bold: false, italic: false, ul: false, ol: false, quote: false, link: false })
const linkOpen = ref(false)
const linkUrl = ref('')
const linkError = ref('')
let saved: Range | null = null

onMounted(() => {
  el.value!.replaceChildren(importHtml(props.html, props.imageUrl))
  try {
    document.execCommand('defaultParagraphSeparator', false, 'p')
  } catch {
    // An old browser: it makes its own kind of lines, which export as they are.
  }
})

function changed() {
  emit('update:html', exportHtml(el.value!))
  track()
}

function inside(tag: string): Element | null {
  const sel = document.getSelection()
  let n: Node | null = sel?.anchorNode ?? null
  while (n && n !== el.value) {
    if (n.nodeType === Node.ELEMENT_NODE && (n as Element).tagName === tag) return n as Element
    n = n.parentNode
  }
  return null
}

function hasSelection(): boolean {
  const sel = document.getSelection()
  return !!sel && sel.rangeCount > 0 && !!el.value?.contains(sel.anchorNode)
}

// track notes what the caret is in, for the pressed state of the buttons.
function track() {
  if (!hasSelection()) return
  active.value = {
    bold: document.queryCommandState('bold'),
    italic: document.queryCommandState('italic'),
    ul: document.queryCommandState('insertUnorderedList'),
    ol: document.queryCommandState('insertOrderedList'),
    quote: !!inside('BLOCKQUOTE'),
    link: !!inside('A'),
  }
}

function run(command: string, value?: string) {
  el.value?.focus()
  document.execCommand(command, false, value)
  changed()
}

function toggleQuote() {
  el.value?.focus()
  if (inside('BLOCKQUOTE')) document.execCommand('outdent')
  else document.execCommand('formatBlock', false, 'blockquote')
  changed()
}

function keep() {
  const sel = document.getSelection()
  saved = sel && hasSelection() ? sel.getRangeAt(0).cloneRange() : null
}

function restore() {
  el.value?.focus()
  if (!saved) return
  const sel = document.getSelection()
  sel?.removeAllRanges()
  sel?.addRange(saved)
}

function openLink() {
  keep()
  linkUrl.value = inside('A')?.getAttribute('href') ?? ''
  linkError.value = ''
  linkOpen.value = true
}

function applyLink() {
  let url = linkUrl.value.trim()
  if (url !== '' && !/^[a-z][a-z0-9+.-]*:/i.test(url)) url = url.includes('@') && !url.includes('/') ? `mailto:${url}` : `https://${url}`
  if (url !== '' && !safeHref(url)) {
    linkError.value = 'A link starts with https://, http:// or mailto:'
    return
  }
  linkOpen.value = false
  restore()
  if (url === '') document.execCommand('unlink')
  else if (document.getSelection()?.isCollapsed && !inside('A')) {
    const a = document.createElement('a')
    a.href = url
    a.textContent = url.replace(/^mailto:/i, '')
    document.getSelection()!.getRangeAt(0).insertNode(a)
    document.getSelection()!.collapse(a.parentNode, [...a.parentNode!.childNodes].indexOf(a) + 1)
  } else document.execCommand('createLink', false, url)
  changed()
}

function onPaste(e: ClipboardEvent) {
  const files = [...(e.clipboardData?.files ?? [])]
  e.preventDefault()
  if (files.length > 0) {
    keep()
    emit('files', files)
    return
  }
  const text = e.clipboardData?.getData('text/plain') ?? ''
  if (text) document.execCommand('insertText', false, text)
  changed()
}

function onDrop(e: DragEvent) {
  // Files are for the composer around us. Anything else comes in as text.
  if (e.dataTransfer?.files.length) return
  e.preventDefault()
  const text = e.dataTransfer?.getData('text/plain') ?? ''
  if (text) run('insertText', text)
}

// insertImage puts an uploaded image where the caret was.
function insertImage(cid: string, url: string) {
  const img = imageElement(cid, url)
  restore()
  const sel = document.getSelection()
  if (sel && hasSelection()) {
    const r = sel.getRangeAt(0)
    r.deleteContents()
    r.insertNode(img)
    r.setStartAfter(img)
    r.collapse(true)
  } else el.value!.appendChild(img)
  changed()
}

function removeImage(cid: string) {
  for (const img of el.value?.querySelectorAll('img') ?? []) if (img.dataset.cid === cid) img.remove()
  changed()
}

defineExpose({ insertImage, removeImage, focus: () => el.value?.focus(), keep })

const buttons = [
  { key: 'bold', icon: 'i-lucide-bold', label: 'Bold', run: () => run('bold') },
  { key: 'italic', icon: 'i-lucide-italic', label: 'Italic', run: () => run('italic') },
  { key: 'ul', icon: 'i-lucide-list', label: 'Bulleted list', run: () => run('insertUnorderedList') },
  { key: 'ol', icon: 'i-lucide-list-ordered', label: 'Numbered list', run: () => run('insertOrderedList') },
  { key: 'quote', icon: 'i-lucide-text-quote', label: 'Quote', run: toggleQuote },
] as const
</script>

<template>
  <div class="flex min-h-0 flex-1 flex-col">
    <div class="flex items-center gap-0.5 border-b border-default px-2 py-1" role="toolbar" aria-label="Formatting">
      <UTooltip v-for="b in buttons" :key="b.key" :text="b.label">
        <UButton
          :icon="b.icon" size="xs" color="neutral" :variant="active[b.key] ? 'soft' : 'ghost'" :aria-label="b.label" :aria-pressed="active[b.key]"
          @mousedown.prevent @click="b.run()"
        />
      </UTooltip>
      <UPopover v-model:open="linkOpen" :content="{ align: 'start' }">
        <UTooltip text="Link">
          <UButton
            icon="i-lucide-link" size="xs" color="neutral" :variant="active.link ? 'soft' : 'ghost'" aria-label="Link" :aria-pressed="active.link"
            @mousedown.prevent @click.prevent="openLink"
          />
        </UTooltip>
        <template #content>
          <form class="flex w-72 flex-col gap-2 p-3" @submit.prevent="applyLink">
            <UFormField label="Link address" :error="linkError || undefined">
              <UInput v-model="linkUrl" placeholder="https://example.org" autofocus class="w-full" autocomplete="off" />
            </UFormField>
            <div class="flex justify-end gap-2">
              <UButton v-if="active.link" label="Remove link" size="xs" color="neutral" variant="ghost" @click="linkUrl = ''; applyLink()" />
              <UButton type="submit" label="Apply" size="xs" />
            </div>
          </form>
        </template>
      </UPopover>
    </div>
    <div
      ref="el" contenteditable="true" role="textbox" aria-multiline="true" :aria-label="label" spellcheck="true" data-testid="rich-body"
      class="rich min-h-0 flex-1 overflow-y-auto overscroll-contain px-3 py-2 text-sm leading-relaxed text-default outline-none"
      @input="changed" @paste="onPaste" @drop="onDrop" @keyup="track" @mouseup="track" @blur="keep"
    />
  </div>
</template>
