<script setup lang="ts">
// The body of one message. An HTML body is the server's sanitised view in
// an iframe that may run no script and has no origin of ours: the page
// cannot reach into it and it cannot reach the session. A text body is
// shown as text.
import { computed, ref, watch } from 'vue'
import { client } from '../app'
import type { Email } from '../jmap/types'
import { fileSize } from '../lib/format'

const props = defineProps<{ email: Email }>()

const html = computed(() => props.email.htmlBody?.some((p) => p.type === 'text/html') ?? false)
const text = computed(() => {
  const values = props.email.bodyValues ?? {}
  return (props.email.textBody ?? [])
    .map((p) => (p.partId !== null ? values[p.partId] : undefined))
    .filter((v) => v !== undefined)
    .map((v) => v.value)
    .join('\n')
})
// The text with its links: web addresses open in a new tab, mail
// addresses in the composer (the app catches mailto: links). The pieces
// are text nodes and anchors made by the template, never markup.
const linked = computed(() => {
  const out: { text: string; href?: string }[] = []
  const re = /\bhttps?:\/\/[^\s<>"')\]]+[^\s<>"')\].,;:!?]|[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+/g
  const t = text.value
  let at = 0
  for (let m = re.exec(t); m; m = re.exec(t)) {
    if (m.index > at) out.push({ text: t.slice(at, m.index) })
    out.push({ text: m[0], href: m[0].includes('://') ? m[0] : `mailto:${m[0]}` })
    at = m.index + m[0].length
  }
  if (at < t.length) out.push({ text: t.slice(at) })
  return out
})
const truncated = computed(() => Object.values(props.email.bodyValues ?? {}).some((v) => v.isTruncated))

// Remote images: left out until the reader asks, per message.
const images = ref(false)
const remote = ref<'blocked' | 'shown' | 'none'>('none')
watch(() => [props.email.id, html.value] as const, async ([id, isHtml]) => {
  images.value = false
  remote.value = 'none'
  if (!isHtml) return
  try {
    const r = await client.remoteImages(id)
    if (props.email.id === id) remote.value = r
  } catch {
    // The view itself shows what is wrong.
  }
}, { immediate: true })

const attachments = computed(() => (props.email.attachments ?? []).filter((a) => a.blobId))
</script>

<template>
  <div class="flex flex-col gap-3">
    <template v-if="html">
      <div v-if="remote === 'blocked' && !images" class="flex flex-wrap items-center gap-2 rounded-md bg-elevated px-3 py-2 text-sm text-muted">
        <UIcon name="i-lucide-image-off" class="size-4 shrink-0" />
        <span class="mr-auto">Remote images are not loaded: they tell the sender you opened this.</span>
        <UButton label="Load remote images" size="xs" color="neutral" variant="outline" data-testid="load-images" @click="images = true" />
      </div>
      <!-- Mail is written for a white page, so the frame is white in dark mode too. -->
      <iframe
        :key="`${email.id}-${images}`" :src="client.viewUrl(email.id, images)" :title="`Message: ${email.subject || '(no subject)'}`"
        sandbox="allow-popups allow-popups-to-escape-sandbox" referrerpolicy="no-referrer" loading="lazy" data-testid="html-view"
        class="h-[65vh] min-h-80 w-full rounded-md border border-default bg-white"
      />
    </template>
    <pre v-else class="whitespace-pre-wrap break-words font-sans text-sm leading-relaxed text-default" data-testid="text-view"><template v-if="!text">(This message has no text.)</template><template v-for="(p, i) in linked" :key="i"><a
      v-if="p.href" :href="p.href" :target="p.href.startsWith('mailto:') ? undefined : '_blank'" rel="noopener noreferrer"
      class="text-primary underline underline-offset-2"
    >{{ p.text }}</a><template v-else>{{ p.text }}</template></template></pre>
    <p v-if="truncated && !html" class="text-xs text-dimmed">
      This message is too long to show in full.
    </p>

    <ul v-if="attachments.length > 0" class="flex flex-wrap gap-2" aria-label="Attachments">
      <li v-for="a in attachments" :key="a.blobId ?? ''">
        <a
          :href="client.downloadUrl(a.blobId!, a.name || 'attachment', a.type)" :download="a.name || 'attachment'"
          class="flex max-w-64 items-center gap-2 rounded-md border border-default px-3 py-2 text-sm hover:bg-elevated focus-visible:outline-2 focus-visible:outline-primary"
          data-testid="attachment"
        >
          <UIcon name="i-lucide-paperclip" class="size-4 shrink-0 text-dimmed" />
          <span class="truncate">{{ a.name || 'attachment' }}</span>
          <span class="shrink-0 text-xs text-dimmed">{{ fileSize(a.size) }}</span>
          <UIcon name="i-lucide-download" class="size-4 shrink-0 text-dimmed" />
        </a>
      </li>
    </ul>
  </div>
</template>
