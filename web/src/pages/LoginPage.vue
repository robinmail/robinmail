<script setup lang="ts">
// The login: address and password, then, for an account with two-step
// login, a code. Or a provider, when the server has any configured.
import { computed, nextTick, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { auth } from '../app'
import { AuthError, explain, login, loginCode, logout, providers as loadProviders, secondProof, type Provider } from '../auth/api'
import { safeNext } from '../router'

const route = useRoute()
const router = useRouter()
const username = ref('')
const password = ref('')
const remember = ref(false)
const code = ref('')
const useRecovery = ref(false)
const busy = ref(false)
const error = ref('')
const providers = ref<Provider[]>([])
// 'totp' once the password was right and the account wants a code.
const step = ref<'password' | 'totp'>(auth.waiting || route.query.step === 'totp' ? 'totp' : 'password')

// What a login through a provider came back with (see oidc.go).
const providerErrors: Record<string, string> = {
  oidc_denied: 'The login at the provider was cancelled.',
  oidc_no_account: 'There is no mailbox here for the account you logged in with at the provider.',
  oidc_unverified: 'The provider has not verified your email address, so it cannot be used to log in here.',
  oidc_unavailable: 'The provider cannot be reached right now. Try again, or log in with your password.',
  oidc_failed: 'The login through the provider did not work. Try again.',
  too_many: 'Too many failed logins. Try again in a few minutes.',
}

onMounted(async () => {
  document.title = 'Log in · robinmail'
  const e = route.query.error
  if (typeof e === 'string') error.value = providerErrors[e] ?? providerErrors.oidc_failed!
  try {
    providers.value = await loadProviders()
  } catch {
    // Without the list there are no buttons; the password still works.
  }
})

async function done() {
  password.value = ''
  code.value = ''
  auth.waiting = false
  await router.replace(safeNext(route.query.next))
}

function loginFailed(e: unknown) {
  if (e instanceof AuthError && e.status === 401) error.value = 'Wrong address or password.'
  else if (e instanceof AuthError && e.status === 429) {
    const minutes = Math.max(1, Math.ceil(e.retryAfter / 60))
    error.value = `Too many failed logins. Try again in ${minutes} ${minutes === 1 ? 'minute' : 'minutes'}.`
  } else if (e instanceof AuthError) error.value = `The server cannot log you in right now (${e.status}).`
  else error.value = 'The server does not answer. Check the connection and try again.'
}

async function submit() {
  if (busy.value) return
  busy.value = true
  error.value = ''
  try {
    const answer = await login(username.value.trim(), password.value, remember.value)
    password.value = ''
    if ('step' in answer) {
      step.value = 'totp'
      await nextTick()
      document.querySelector<HTMLInputElement>('input[name="code"]')?.focus()
    } else {
      auth.account = answer
      await done()
    }
  } catch (e) {
    loginFailed(e)
  } finally {
    busy.value = false
  }
}

async function submitCode() {
  if (busy.value) return
  busy.value = true
  error.value = ''
  try {
    auth.account = await loginCode(useRecovery.value ? { recoveryCode: code.value.trim() } : secondProof(code.value))
    await done()
  } catch (e) {
    code.value = ''
    if (e instanceof AuthError && e.status === 401 && e.message !== 'wrong code') {
      // The login that waited is gone: five minutes passed, or it was used.
      step.value = 'password'
      error.value = 'That took too long. Log in again.'
    } else if (e instanceof AuthError && e.status === 401) {
      error.value = useRecovery.value ? 'That recovery code is not right, or was used before.' : 'That code is not right. Codes change every 30 seconds and work once.'
    } else error.value = explain(e)
  } finally {
    busy.value = false
  }
}

async function startOver() {
  error.value = ''
  code.value = ''
  useRecovery.value = false
  step.value = 'password'
  auth.waiting = false
  try {
    await logout() // forgets the login that waits
  } catch {
    // it ends by itself in five minutes
  }
}

const providerLink = computed(() => (p: Provider) => `/auth/oidc/${encodeURIComponent(p.name)}/start${remember.value ? '?remember=1' : ''}`)
</script>

<template>
  <main class="min-h-full flex flex-col items-center justify-center gap-6 p-4 bg-muted">
    <div class="flex items-center gap-2 text-highlighted">
      <img src="/favicon.svg" alt="" class="size-8">
      <span class="text-2xl font-semibold tracking-tight">robinmail</span>
    </div>
    <UCard class="w-full max-w-sm">
      <form v-if="step === 'password'" class="flex flex-col gap-4" data-testid="login-form" @submit.prevent="submit">
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
        <UCheckbox v-model="remember" name="remember" label="Remember this device" description="Stay logged in for up to 30 days. Not on a shared computer." />
        <UButton type="submit" block size="lg" :loading="busy" label="Log in" />
        <template v-if="providers.length">
          <USeparator label="or" />
          <UButton
            v-for="p in providers" :key="p.name" :to="providerLink(p)" external block size="lg" color="neutral" variant="subtle"
            icon="i-lucide-key-round" :label="`Log in with ${p.label}`" :data-testid="`provider-${p.name}`"
          />
        </template>
      </form>

      <form v-else class="flex flex-col gap-4" data-testid="code-form" @submit.prevent="submitCode">
        <h1 class="text-lg font-semibold text-highlighted">
          Two-step login
        </h1>
        <UAlert v-if="error" color="error" variant="subtle" icon="i-lucide-circle-alert" :title="error" role="alert" />
        <UFormField v-if="!useRecovery" label="Code" name="code" help="The six digits your authenticator app shows for robinmail.">
          <UInput
            v-model="code" name="code" inputmode="numeric" autocomplete="one-time-code" pattern="[0-9 ]*" maxlength="9" required autofocus
            placeholder="123456" class="w-full" size="lg" :ui="{ base: 'tracking-widest' }"
          />
        </UFormField>
        <UFormField v-else label="Recovery code" name="code" help="One of the codes you saved when you turned two-step login on. Each works once.">
          <UInput
            v-model="code" name="code" autocomplete="off" autocapitalize="off" spellcheck="false" maxlength="40" required autofocus
            placeholder="abcd-efgh-ijkl-mnop" class="w-full" size="lg"
          />
        </UFormField>
        <UButton type="submit" block size="lg" :loading="busy" label="Log in" />
        <div class="flex items-center justify-between gap-2 text-sm">
          <UButton
            variant="link" color="neutral" :padded="false" :label="useRecovery ? 'Use a code from the app' : 'Use a recovery code'" data-testid="toggle-recovery"
            @click="useRecovery = !useRecovery; code = ''; error = ''"
          />
          <UButton variant="link" color="neutral" :padded="false" label="Start over" @click="startOver" />
        </div>
      </form>
    </UCard>
    <UColorModeButton variant="ghost" color="neutral" />
  </main>
</template>
