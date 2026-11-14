<script setup lang="ts">
// How full the account is (JMAP Quota, RFC 9425): a bar per limit, only
// when the account has limits.
import { computed } from 'vue'
import { store } from '../app'
import { octets } from '../lib/format'

const meters = computed(() =>
  store.state.quota
    .filter((q) => q.hardLimit > 0)
    .map((q) => {
      const part = Math.min(1, q.used / q.hardLimit)
      const text = q.resourceType === 'octets'
        ? `${octets(q.used)} of ${octets(q.hardLimit)}`
        : `${q.used.toLocaleString()} of ${q.hardLimit.toLocaleString()} messages`
      return { id: q.id, name: q.name, text, percent: Math.round(part * 100), full: part >= 1, near: part >= 0.9 }
    }),
)
</script>

<template>
  <div v-if="meters.length" class="flex flex-col gap-2 px-1" data-testid="quota">
    <div v-for="m in meters" :key="m.id" :data-quota="m.id">
      <div class="flex items-baseline justify-between gap-2 text-xs">
        <span class="text-muted">{{ m.name }}</span>
        <span :class="m.full ? 'font-medium text-error' : m.near ? 'text-warning' : 'text-dimmed'">{{ m.text }}</span>
      </div>
      <div
        class="mt-1 h-1.5 overflow-hidden rounded-full bg-elevated" role="progressbar" :aria-label="`${m.name}: ${m.text}`"
        aria-valuemin="0" aria-valuemax="100" :aria-valuenow="m.percent"
      >
        <div class="h-full rounded-full" :class="m.full ? 'bg-error' : m.near ? 'bg-warning' : 'bg-primary'" :style="{ width: `${m.percent}%` }" />
      </div>
      <p v-if="m.full" class="mt-1 text-xs text-error">
        Full: new mail is refused until you delete some.
      </p>
    </div>
  </div>
</template>
