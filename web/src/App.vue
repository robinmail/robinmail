<script setup lang="ts">
import { auth } from './app'
</script>

<template>
  <!-- Notices sit top right: the bottom right is where messages are written. -->
  <UApp :toaster="{ position: 'top-right' }" :tooltip="{ delayDuration: 400 }">
    <RouterView />
    <ComposerDock v-if="auth.account" />
  </UApp>
</template>
