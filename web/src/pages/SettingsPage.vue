<script setup lang="ts">
// Identities: the addresses to send as. The account's own address is
// always there; others are that address with a +tag, or an alias that
// delivers to the account (phase 8), each with its own name, reply-to and
// signature.
import { computed, onMounted, ref } from 'vue'
import { auth, compose, loadAccount } from '../app'
import { addresses } from '../auth/api'
import { formatAddress, isValidEmail, parseAddresses } from '../compose/address'
import type { Identity } from '../jmap/types'

const loadError = ref('')
async function load() {
  loadError.value = ''
  try {
    await loadAccount()
  } catch (e) {
    if (auth.account) loadError.value = e instanceof Error ? e.message : String(e)
  }
  // Without the list the page still works for the account's own address.
  try {
    sendAs.value = await addresses()
  } catch {
    sendAs.value = []
  }
}
onMounted(() => {
  document.title = 'Identities · robinmail'
  void load()
})

// The addresses a new identity can be for: the account's own, and the
// aliases that deliver to it. The chosen one stands around the tag.
const own = computed(() => compose.state.identities.find((i) => !i.mayDelete)?.email ?? auth.account?.username ?? '')
const sendAs = ref<string[]>([])
const baseAddress = ref('')
const bases = computed(() => (sendAs.value.length ? sendAs.value : [own.value]))
const base = computed(() => {
  const chosen = bases.value.includes(baseAddress.value) ? baseAddress.value : own.value
  const at = chosen.lastIndexOf('@')
  return { address: chosen, local: chosen.slice(0, at), domain: chosen.slice(at + 1) }
})
// An alias can be used as it is; the account's own address has its
// identity already, so a new one for it needs a tag.
const tagNeeded = computed(() => compose.state.identities.some((i) => i.email === base.value.address))

type Dialog = { kind: 'add' } | { kind: 'edit'; identity: Identity } | { kind: 'remove'; identity: Identity }
const dialog = ref<Dialog | null>(null)
const open = computed({ get: () => dialog.value !== null, set: (v) => { if (!v) dialog.value = null } })
const form = ref({ tag: '', name: '', replyTo: '', signature: '' })
const busy = ref(false)
const error = ref('')

function show(d: Dialog) {
  dialog.value = d
  baseAddress.value = own.value
  error.value = ''
  form.value = d.kind === 'edit'
    ? { tag: '', name: d.identity.name, replyTo: (d.identity.replyTo ?? []).map(formatAddress).join(', '), signature: d.identity.textSignature }
    : { tag: '', name: compose.state.identities[0]?.name ?? '', replyTo: '', signature: '' }
}

const title = computed(() => {
  const d = dialog.value
  if (!d) return ''
  return d.kind === 'add' ? 'New identity' : d.kind === 'edit' ? `Edit ${d.identity.email}` : `Remove ${d.identity.email}?`
})
const newAddress = computed(() => (form.value.tag.trim() ? `${base.value.local}+${form.value.tag.trim()}@${base.value.domain}` : base.value.address))

async function submit() {
  const d = dialog.value
  if (!d || busy.value) return
  error.value = ''
  const replyTo = parseAddresses(form.value.replyTo)
  if (d.kind !== 'remove') {
    if (replyTo.invalid.length) {
      error.value = `Reply-to: “${replyTo.invalid[0]}” is not an email address.`
      return
    }
    if (d.kind === 'add' && ((form.value.tag.trim() === '' && tagNeeded.value) || !isValidEmail(newAddress.value))) {
      error.value = 'Give the address a tag of letters, digits, dots or dashes.'
      return
    }
  }
  const input = { name: form.value.name.trim(), replyTo: replyTo.addresses, textSignature: form.value.signature.replace(/\s+$/, '') }
  busy.value = true
  try {
    if (d.kind === 'add') await compose.createIdentity(newAddress.value, input)
    else if (d.kind === 'edit') await compose.updateIdentity(d.identity.id, input)
    else await compose.destroyIdentity(d.identity.id)
    dialog.value = null
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
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
        Identities
      </h1>
      <SettingsTabs class="ml-auto" />
    </header>

    <main class="mx-auto flex w-full max-w-2xl flex-1 flex-col gap-4 p-4 sm:p-6">
      <div v-if="loadError" class="flex flex-col items-center gap-3 pt-16 text-center">
        <UIcon name="i-lucide-cloud-off" class="size-10 text-dimmed" />
        <p class="text-sm text-muted">
          The identities could not be loaded.<br>{{ loadError }}
        </p>
        <UButton label="Try again" @click="load" />
      </div>
      <template v-else>
        <p class="text-sm text-muted">
          An identity is an address you send from, with a name and a signature. Besides your own address you can use it
          with a tag, like <span class="whitespace-nowrap font-medium text-default">{{ base.local }}+shop@{{ base.domain }}</span>: mail to a tagged address
          arrives in this account too.
        </p>

        <div v-if="!compose.state.identitiesLoaded" class="flex flex-col gap-3" aria-busy="true">
          <USkeleton v-for="n in 2" :key="n" class="h-24 w-full" />
        </div>
        <ul v-else class="flex flex-col gap-3" aria-label="Identities">
          <li v-for="i in compose.state.identities" :key="i.id" class="rounded-lg border border-default p-4" data-testid="identity">
            <div class="flex items-start gap-2">
              <div class="min-w-0 flex-1">
                <p class="truncate font-medium text-highlighted">
                  {{ i.name || '(no name)' }}
                </p>
                <p class="truncate text-sm text-muted">
                  {{ i.email }}
                </p>
              </div>
              <UButton label="Edit" icon="i-lucide-pencil" size="sm" color="neutral" variant="ghost" :aria-label="`Edit ${i.email}`" @click="show({ kind: 'edit', identity: i })" />
              <UButton
                v-if="i.mayDelete" icon="i-lucide-trash-2" size="sm" color="neutral" variant="ghost" :aria-label="`Remove ${i.email}`"
                @click="show({ kind: 'remove', identity: i })"
              />
            </div>
            <dl class="mt-2 grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-sm">
              <template v-if="i.replyTo?.length">
                <dt class="text-dimmed">
                  Reply-to
                </dt>
                <dd class="min-w-0 break-words text-default">
                  {{ i.replyTo.map(formatAddress).join(', ') }}
                </dd>
              </template>
              <dt class="text-dimmed">
                Signature
              </dt>
              <dd class="min-w-0 whitespace-pre-wrap break-words" :class="i.textSignature ? 'text-default' : 'text-dimmed'">
                {{ i.textSignature || 'None' }}
              </dd>
            </dl>
          </li>
        </ul>
        <div>
          <UButton label="Add an identity" icon="i-lucide-plus" color="neutral" variant="subtle" :disabled="!compose.state.identitiesLoaded" data-testid="add-identity" @click="show({ kind: 'add' })" />
        </div>
      </template>
    </main>

    <UModal v-model:open="open" :title="title" :description="dialog?.kind === 'remove' ? 'Mail sent to this address still arrives.' : 'How your mail shows to the people you write to.'">
      <template #body>
        <form id="identity-form" class="flex flex-col gap-4" @submit.prevent="submit">
          <UAlert v-if="error" color="error" variant="subtle" icon="i-lucide-circle-alert" :title="error" role="alert" />
          <p v-if="dialog?.kind === 'remove'" class="text-sm">
            You will no longer be able to send from this address. Messages you sent from it stay.
          </p>
          <template v-else>
            <UFormField v-if="dialog?.kind === 'add' && bases.length > 1" label="Send as" name="base" help="Your own address, or an alias that delivers to you.">
              <USelect v-model="baseAddress" :items="bases" name="base" class="w-full" data-testid="identity-base" />
            </UFormField>
            <UFormField v-if="dialog?.kind === 'add'" label="Address" name="tag" :help="newAddress">
              <div class="flex items-center gap-1 text-sm">
                <span class="shrink-0 text-muted">{{ base.local }}+</span>
                <UInput v-model="form.tag" name="tag" autofocus autocomplete="off" autocapitalize="off" spellcheck="false" maxlength="40" :placeholder="tagNeeded ? 'tag' : 'tag (optional)'" class="min-w-0 flex-1" />
                <span class="min-w-0 truncate text-muted">@{{ base.domain }}</span>
              </div>
            </UFormField>
            <UFormField label="Name" name="name" help="Shown next to the address.">
              <UInput v-model="form.name" name="name" maxlength="200" class="w-full" autocomplete="name" />
            </UFormField>
            <UFormField label="Reply-to" name="replyTo" help="Where answers should go, if not to this address. Optional.">
              <UInput v-model="form.replyTo" name="replyTo" class="w-full" autocomplete="off" inputmode="email" />
            </UFormField>
            <UFormField label="Signature" name="signature" help="Plain text, put under a new message.">
              <UTextarea v-model="form.signature" name="signature" :rows="4" autoresize class="w-full" />
            </UFormField>
          </template>
        </form>
      </template>
      <template #footer>
        <div class="flex w-full justify-end gap-2">
          <UButton label="Cancel" color="neutral" variant="ghost" @click="dialog = null" />
          <UButton
            type="submit" form="identity-form" :loading="busy" :color="dialog?.kind === 'remove' ? 'error' : 'primary'"
            :label="dialog?.kind === 'remove' ? 'Remove' : dialog?.kind === 'edit' ? 'Save' : 'Add'"
          />
        </div>
      </template>
    </UModal>
  </div>
</template>
