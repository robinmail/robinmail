<script setup lang="ts">
// Vacation: the automatic reply of the account (JMAP VacationResponse,
// RFC 8621 8). The server sends it at most once a week to each sender, and
// never to lists, notifications or other automatic mail.
import { computed, onMounted, ref } from 'vue'
import { auth, client, loadAccount } from '../app'
import { MethodError } from '../jmap/client'
import { fromLocalInput, toLocalInput } from '../lib/localDate'

const toast = useToast()
const loadError = ref('')
const loading = ref(true)
const busy = ref(false)
const error = ref('')

const blank = () => ({ enabled: false, from: '', to: '', subject: '', text: '' })
const form = ref(blank())
const saved = ref(blank())
// An HTML body another client set stays as it is unless the text changes.
const html = ref<string | null>(null)
const changed = computed(() => JSON.stringify(form.value) !== JSON.stringify(saved.value))

async function load() {
  loadError.value = ''
  loading.value = true
  try {
    await loadAccount()
    const b = client.batch()
    const get = b.call('VacationResponse/get', { ids: ['singleton'] })
    const v = (await b.send()).get(get).list[0]
    if (v) {
      form.value = { enabled: v.isEnabled, from: toLocalInput(v.fromDate), to: toLocalInput(v.toDate), subject: v.subject ?? '', text: v.textBody ?? '' }
      html.value = v.htmlBody
    }
    saved.value = { ...form.value }
  } catch (e) {
    if (auth.account) loadError.value = e instanceof Error ? e.message : String(e)
  } finally {
    loading.value = false
  }
}
onMounted(() => {
  document.title = 'Vacation · robinmail'
  void load()
})

async function save() {
  if (busy.value) return
  error.value = ''
  const f = form.value
  const from = fromLocalInput(f.from)
  const to = fromLocalInput(f.to)
  if (from && to && from >= to) {
    error.value = 'The last day must be after the first.'
    return
  }
  busy.value = true
  try {
    const b = client.batch()
    const set = b.call('VacationResponse/set', {
      update: {
        singleton: {
          isEnabled: f.enabled,
          fromDate: from,
          toDate: to,
          subject: f.subject.trim() || null,
          textBody: f.text.trim() ? f.text : null,
          // A text typed here replaces an HTML body from elsewhere: the
          // two would otherwise say different things.
          ...(f.text !== saved.value.text ? { htmlBody: null } : {}),
        },
      },
    })
    const r = (await b.send()).get(set)
    const failed = r.notUpdated?.singleton
    if (failed) {
      error.value = failed.description || failed.type
      return
    }
    await load()
    toast.add({ title: f.enabled ? 'The automatic reply is on' : 'The automatic reply is off', icon: 'i-lucide-check' })
  } catch (e) {
    error.value = e instanceof MethodError ? e.description || e.type : e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="flex min-h-full flex-col bg-default">
    <header class="flex h-(--ui-header-height) shrink-0 items-center gap-2 border-b border-default px-3 sm:px-6">
      <UButton icon="i-lucide-arrow-left" color="neutral" variant="ghost" aria-label="Back to the mail" :to="{ name: 'mail' }" />
      <h1 class="font-semibold text-highlighted">
        Vacation
      </h1>
      <SettingsTabs class="ml-auto" />
    </header>

    <main class="mx-auto flex w-full max-w-2xl flex-1 flex-col gap-4 p-4 sm:p-6">
      <div v-if="loadError" class="flex flex-col items-center gap-3 pt-16 text-center">
        <UIcon name="i-lucide-cloud-off" class="size-10 text-dimmed" />
        <p class="text-sm text-muted">
          The automatic reply could not be loaded.<br>{{ loadError }}
        </p>
        <UButton label="Try again" @click="load" />
      </div>
      <div v-else-if="loading" class="flex flex-col gap-3" aria-busy="true">
        <USkeleton v-for="n in 3" :key="n" class="h-12 w-full" />
      </div>
      <form v-else class="flex flex-col gap-4" @submit.prevent="save">
        <p class="text-sm text-muted">
          While it is on, people who write to you get this reply: once a week each, and never mailing lists,
          newsletters or other automatic mail. Their message arrives as usual.
        </p>
        <UAlert v-if="error" color="error" variant="subtle" icon="i-lucide-circle-alert" :title="error" role="alert" />
        <USwitch v-model="form.enabled" label="Send an automatic reply" data-testid="vacation-enabled" />
        <div class="grid gap-4 sm:grid-cols-2">
          <UFormField label="From" name="from" help="Empty: from now on.">
            <UInput v-model="form.from" type="datetime-local" name="from" class="w-full" />
          </UFormField>
          <UFormField label="Until" name="to" help="Empty: until you turn it off.">
            <UInput v-model="form.to" type="datetime-local" name="to" class="w-full" />
          </UFormField>
        </div>
        <UFormField label="Subject" name="subject" help="Empty: “Auto:” and the subject of the message you got.">
          <UInput v-model="form.subject" name="subject" maxlength="200" class="w-full" autocomplete="off" />
        </UFormField>
        <UFormField label="Message" name="text">
          <UTextarea v-model="form.text" name="text" :rows="6" autoresize class="w-full" placeholder="I am away until Monday and will answer when I am back." />
        </UFormField>
        <p v-if="html !== null && form.text === saved.text" class="text-xs text-dimmed">
          Another program gave this reply a formatted version too. It is kept until you change the message here.
        </p>
        <div>
          <UButton type="submit" label="Save" :loading="busy" :disabled="!changed" data-testid="vacation-save" />
        </div>
      </form>
    </main>
  </div>
</template>
