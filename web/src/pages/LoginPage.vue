<script setup lang="ts">
import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { auth } from '../app'
import { LoginError, login } from '../jmap/client'
import { safeNext } from '../router'

const route = useRoute()
const router = useRouter()
const username = ref('')
const password = ref('')
const busy = ref(false)
const error = ref('')

async function submit() {
  if (busy.value) return
  busy.value = true
  error.value = ''
  try {
    auth.account = await login(username.value.trim(), password.value)
    password.value = ''
    await router.replace(safeNext(route.query.next))
  } catch (e) {
    if (e instanceof LoginError && e.status === 401) error.value = 'Wrong address or password.'
    else if (e instanceof LoginError && e.status === 429) {
      const minutes = Math.max(1, Math.ceil(e.retryAfter / 60))
      error.value = `Too many failed logins. Try again in ${minutes} ${minutes === 1 ? 'minute' : 'minutes'}.`
    } else if (e instanceof LoginError) error.value = `The server cannot log you in right now (${e.status}).`
    else error.value = 'The server does not answer. Check the connection and try again.'
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <main class="min-h-full flex flex-col items-center justify-center gap-6 p-4 bg-muted">
    <div class="flex items-center gap-2 text-highlighted">
      <img src="/favicon.svg" alt="" class="size-8">
      <span class="text-2xl font-semibold tracking-tight">robinmail</span>
    </div>
    <UCard class="w-full max-w-sm">
      <form class="flex flex-col gap-4" @submit.prevent="submit">
        <h1 class="text-lg font-semibold text-highlighted">
          Log in
        </h1>
        <UAlert v-if="error" color="error" variant="subtle" icon="i-lucide-circle-alert" :title="error" role="alert" />
        <UFormField label="Email address" name="username">
          <UInput
            v-model="username" type="email" name="username" autocomplete="username" required autofocus
            placeholder="you@example.com" class="w-full" size="lg"
          />
        </UFormField>
        <UFormField label="Password" name="password">
          <UInput
            v-model="password" type="password" name="password" autocomplete="current-password" required
            class="w-full" size="lg"
          />
        </UFormField>
        <UButton type="submit" block size="lg" :loading="busy" label="Log in" />
      </form>
    </UCard>
    <UColorModeButton variant="ghost" color="neutral" />
  </main>
</template>
