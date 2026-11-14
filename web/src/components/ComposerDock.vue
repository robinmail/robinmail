<script setup lang="ts">
// Where the open composers sit: along the bottom right of the window, or
// one at a time over the whole screen on a phone. It also owns what goes
// with writing mail wherever the user is in the app: the "undo" notice
// after a send, mailto: links, and the warning before the tab closes
// with something unsent.
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { compose } from '../app'
import { parseMailto } from '../compose/address'
import type { Sent } from '../store/compose'

const toast = useToast()

try {
  compose.rich = localStorage.getItem('robinmail-rich') === '1'
} catch {
  // Storage is off: plain text.
}
compose.onError = (message) => toast.add({ title: message, color: 'error', icon: 'i-lucide-circle-alert' })

// How many composers fit next to each other.
const PANEL = 36 * 16 + 12
const width = ref(window.innerWidth)
const phone = ref(window.innerWidth < 640)
function onResize() {
  width.value = window.innerWidth
  phone.value = window.innerWidth < 640
}
const room = () => (phone.value ? 1 : Math.max(1, Math.floor((width.value - 32) / PANEL)))

// When one more opens than fit, the ones opened longest ago are minimised.
let before: number[] = []
watch(
  () => [compose.state.composers.filter((c) => !c.minimized).map((c) => c.key), width.value] as const,
  ([open]) => {
    const fresh = open.filter((k) => !before.includes(k))
    let over = open.length - room()
    for (const c of compose.state.composers) {
      if (over <= 0) break
      if (c.minimized || fresh.includes(c.key)) continue
      c.minimized = true
      over--
    }
    before = compose.state.composers.filter((c) => !c.minimized).map((c) => c.key)
  },
  { deep: true },
)

// --- undo ---

// A send removes its composer, so the notice is shown from here.
const noticed = new Set<Sent>()
watch(() => compose.state.sent.length, () => {
  for (const s of compose.state.sent) {
    if (noticed.has(s)) continue
    noticed.add(s)
    onSent(s)
  }
})

function onSent(s: Sent) {
  const ms = Math.min(30_000, Math.max(3000, s.until - Date.now()))
  const id = `sent-${s.key}-${s.emailId}`
  toast.add({
    id, title: 'Message sent', description: `You can take it back for ${Math.round(ms / 1000)} seconds.`, icon: 'i-lucide-send', duration: ms,
    actions: [{ label: 'Undo', color: 'neutral', variant: 'outline', onClick: () => void undo(s, id) }],
  })
  setTimeout(() => {
    compose.forget(s)
    noticed.delete(s)
  }, ms + 1000)
}

async function undo(s: Sent, id: string) {
  toast.remove(id)
  try {
    await compose.undo(s)
    toast.add({ title: 'The message was not sent. It is a draft again.', icon: 'i-lucide-undo-2' })
  } catch (e) {
    toast.add({ title: e instanceof Error ? e.message : String(e), color: 'error', icon: 'i-lucide-circle-alert' })
  }
}

// --- mailto: links anywhere in the app ---

function onClick(e: MouseEvent) {
  if (e.defaultPrevented || e.button !== 0) return
  const a = (e.target as Element | null)?.closest?.('a[href]')
  const m = a ? parseMailto(a.getAttribute('href') ?? '') : null
  if (!m) return
  e.preventDefault()
  compose.compose(m)
}

// --- leaving ---

function beforeUnload(e: BeforeUnloadEvent) {
  if (!compose.hasUnsaved()) return
  e.preventDefault()
  e.returnValue = ''
}
// The page is put away (tab switch, phone lock): save now, it may not come back.
function onHide() {
  if (document.visibilityState !== 'hidden') return
  for (const c of compose.state.composers) if (c.version !== c.savedVersion) void compose.save(c)
}

onMounted(() => {
  window.addEventListener('resize', onResize)
  window.addEventListener('beforeunload', beforeUnload)
  document.addEventListener('click', onClick)
  document.addEventListener('visibilitychange', onHide)
})
onBeforeUnmount(() => {
  window.removeEventListener('resize', onResize)
  window.removeEventListener('beforeunload', beforeUnload)
  document.removeEventListener('click', onClick)
  document.removeEventListener('visibilitychange', onHide)
})
</script>

<template>
  <div
    v-if="compose.state.composers.length > 0" data-testid="composer-dock"
    class="pointer-events-none fixed inset-x-0 bottom-0 flex flex-col-reverse items-stretch sm:left-auto sm:right-4 sm:flex-row-reverse sm:items-end sm:gap-3"
  >
    <ComposerPanel v-for="c in compose.state.composers" :key="c.key" :composer="c" :phone="phone" />
  </div>
</template>
