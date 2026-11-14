<script setup lang="ts">
// The settings pages, as links.
import { useRoute } from 'vue-router'

const route = useRoute()
const tabs = [
  { name: 'settings', label: 'Identities', icon: 'i-lucide-at-sign' },
  { name: 'filters', label: 'Filters', icon: 'i-lucide-list-filter' },
  { name: 'security', label: 'Security', icon: 'i-lucide-shield-check' },
]
</script>

<template>
  <nav class="flex items-center gap-1" aria-label="Settings">
    <UButton
      v-for="t in tabs" :key="t.name" :to="{ name: t.name }" :label="t.label" :icon="t.icon" size="sm" color="neutral"
      :variant="route.name === t.name ? 'soft' : 'ghost'" :aria-current="route.name === t.name ? 'page' : undefined"
    />
  </nav>
</template>
