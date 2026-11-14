<script setup lang="ts">
// Filters: what happens to mail when it arrives. The server runs one Sieve
// script per account (RFC 5228, managed over JMAP, RFC 9661). This page
// shows that script as a list of simple rules when it is one the page
// wrote itself (sieve/rules.ts), and as text otherwise; the text can be
// edited either way.
import { computed, onMounted, ref } from 'vue'
import { auth, client, loadAccount, store } from '../app'
import { MethodError } from '../jmap/client'
import type { SetError, SieveScript } from '../jmap/types'
import { describe, generate, parse, problem, type Action, type Field, type Rule } from '../sieve/rules'

const toast = useToast()
// The name of the script the page makes when the account has none.
const NAME = 'Filters'

const loadError = ref('')
const loading = ref(true)
const scripts = ref<SieveScript[]>([])
// The script as the server has it, with LF line ends.
const text = ref('')
const draft = ref('')
const tab = ref<'rules' | 'script'>('rules')
const busy = ref(false)
const saveError = ref('')

// The script the page is about: the active one, else the one it made
// before, else the first.
const current = computed(() => scripts.value.find((s) => s.isActive) ?? scripts.value.find((s) => s.name === NAME) ?? scripts.value[0] ?? null)
const enabled = computed(() => current.value?.isActive ?? false)
const rules = computed(() => (current.value ? parse(text.value) : []))

// load reads the scripts. first is the load of the page itself: only then
// does a script that is not rules open as text, so that a save does not
// change the tab under the user's hands.
async function load(first = false) {
  loadError.value = ''
  loading.value = true
  try {
    await loadAccount()
    // The mailboxes a rule can name, as they are now: this page has no
    // push of its own.
    await store.refresh()
    const b = client.batch()
    const get = b.call('SieveScript/get', { ids: null })
    scripts.value = (await b.send()).get(get).list
    text.value = ''
    if (current.value) {
      const res = await fetch(client.downloadUrl(current.value.blobId, 'filters.sieve', 'application/sieve'), { credentials: 'same-origin' })
      if (!res.ok) throw new Error(`The script could not be read (HTTP ${res.status}).`)
      text.value = (await res.text()).replace(/\r\n/g, '\n')
    }
    draft.value = text.value
    if (first && rules.value === null) tab.value = 'script'
  } catch (e) {
    if (auth.account) loadError.value = e instanceof Error ? e.message : String(e)
  } finally {
    loading.value = false
  }
}
onMounted(() => {
  document.title = 'Filters · robinmail'
  void load(true)
})

function explain(e: SetError): string {
  switch (e.type) {
    case 'invalidSieve':
      return `The script is not valid: ${e.description ?? 'the server did not say why'}`
    case 'tooLarge':
      return 'The script is too large.'
    case 'overQuota':
      return 'The account has too many scripts.'
    default:
      return e.description || e.type
  }
}

// save stores a script as the account's filter. Without a script yet it
// makes one and turns it on; an existing one keeps being on or off.
async function save(script: string): Promise<boolean> {
  if (busy.value) return false
  busy.value = true
  saveError.value = ''
  try {
    const up = await client.upload(new Blob([script.replace(/\r?\n/g, '\r\n')]), 'application/sieve')
    const b = client.batch()
    const cur = current.value
    const set = cur
      ? b.call('SieveScript/set', { update: { [cur.id]: { blobId: up.blobId } } })
      : b.call('SieveScript/set', { create: { filters: { name: NAME, blobId: up.blobId } }, onSuccessActivateScript: '#filters' })
    const r = (await b.send()).get(set)
    const failed = cur ? r.notUpdated?.[cur.id] : r.notCreated?.filters
    if (failed) {
      saveError.value = explain(failed)
      return false
    }
    await load()
    return true
  } catch (e) {
    saveError.value = e instanceof MethodError ? e.description || e.type : e instanceof Error ? e.message : String(e)
    return false
  } finally {
    busy.value = false
  }
}

async function setEnabled(on: boolean) {
  const cur = current.value
  if (!cur || busy.value) return
  busy.value = true
  saveError.value = ''
  try {
    const b = client.batch()
    const set = b.call('SieveScript/set', on ? { onSuccessActivateScript: cur.id } : { onSuccessDeactivateScript: true })
    ;(await b.send()).get(set)
    await load()
    toast.add({ title: on ? 'Filters are on' : 'Filters are off', description: on ? undefined : 'New mail goes to the Inbox as it is.', icon: 'i-lucide-check' })
  } catch (e) {
    saveError.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}

// --- the rules ---

// The mailboxes a rule can file into, by the name Sieve knows them by:
// the levels joined by "/", the inbox as INBOX.
const mailboxes = computed(() => {
  const all = store.state.mailboxes
  const path = (id: string, depth = 0): string => {
    const m = all[id]
    if (!m || depth > 20) return ''
    const own = m.role === 'inbox' && !m.parentId ? 'INBOX' : m.name
    return m.parentId ? `${path(m.parentId, depth + 1)}/${own}` : own
  }
  return Object.values(all)
    .filter((m) => m.role !== 'drafts' && m.role !== 'sent')
    .map((m) => ({ label: path(m.id) === 'INBOX' ? 'Inbox' : path(m.id), value: path(m.id) }))
    .sort((a, b) => a.label.localeCompare(b.label))
})

const fields: { label: string; value: Field }[] = [
  { label: 'From', value: 'from' },
  { label: 'To or Cc', value: 'to' },
  { label: 'Subject', value: 'subject' },
]

interface Form {
  name: string
  match: 'all' | 'any'
  conditions: { field: Field; text: string }[]
  move: boolean
  moveTo: string
  label: boolean
  labelWith: string
  read: boolean
  forward: boolean
  forwardTo: string
  remove: boolean
}

const editing = ref<number | null>(null) // index of the rule in the dialog; -1 for a new one
const form = ref<Form>(blank())
const formError = ref('')
const open = computed({ get: () => editing.value !== null, set: (v) => { if (!v) editing.value = null } })

function blank(): Form {
  return { name: '', match: 'all', conditions: [{ field: 'from', text: '' }], move: false, moveTo: '', label: false, labelWith: '', read: false, forward: false, forwardTo: '', remove: false }
}

function edit(index: number) {
  formError.value = ''
  const r = index >= 0 ? rules.value?.[index] : undefined
  const f = blank()
  if (r) {
    f.name = r.name
    f.match = r.match
    f.conditions = r.conditions.map((c) => ({ ...c }))
    for (const a of r.actions) {
      if (a.kind === 'move') [f.move, f.moveTo] = [true, a.mailbox]
      if (a.kind === 'label') [f.label, f.labelWith] = [true, a.mailbox]
      if (a.kind === 'read') f.read = true
      if (a.kind === 'forward') [f.forward, f.forwardTo] = [true, a.address]
      if (a.kind === 'delete') f.remove = true
    }
  }
  form.value = f
  editing.value = index
}

function fromForm(f: Form): Rule {
  const actions: Action[] = []
  if (f.read) actions.push({ kind: 'read' })
  if (f.label) actions.push({ kind: 'label', mailbox: f.labelWith })
  if (f.forward) actions.push({ kind: 'forward', address: f.forwardTo.trim() })
  if (f.move) actions.push({ kind: 'move', mailbox: f.moveTo })
  if (f.remove) actions.push({ kind: 'delete' })
  return { name: f.name.trim(), match: f.match, conditions: f.conditions.map((c) => ({ field: c.field, text: c.text.trim() })), actions }
}

async function saveRules(next: Rule[], done: string) {
  if (await save(generate(next))) toast.add({ title: done, icon: 'i-lucide-check' })
}

async function submitRule() {
  const rule = fromForm(form.value)
  formError.value = problem(rule)
  if (formError.value) return
  const next = [...(rules.value ?? [])]
  if (editing.value !== null && editing.value >= 0) next[editing.value] = rule
  else next.push(rule)
  const isNew = editing.value === -1
  editing.value = null
  await saveRules(next, isNew ? `Added “${rule.name}”` : `Saved “${rule.name}”`)
}

function removeRule(index: number) {
  const list = rules.value ?? []
  void saveRules(list.filter((_, i) => i !== index), `Removed “${list[index]?.name}”`)
}

function moveRule(index: number, by: -1 | 1) {
  const next = [...(rules.value ?? [])]
  const to = index + by
  if (to < 0 || to >= next.length) return
  ;[next[index], next[to]] = [next[to]!, next[index]!]
  void saveRules(next, 'Order changed')
}

// --- the script as text ---

async function saveScript() {
  if (await save(draft.value)) toast.add({ title: 'Script saved', icon: 'i-lucide-check' })
}

// Replace a script written by hand with an empty list of rules.
async function startOver() {
  if (await save(generate([]))) tab.value = 'rules'
}
</script>

<template>
  <div class="flex min-h-full flex-col bg-default">
    <header class="flex h-(--ui-header-height) shrink-0 items-center gap-2 border-b border-default px-3 sm:px-6">
      <UButton icon="i-lucide-arrow-left" color="neutral" variant="ghost" aria-label="Back to the mail" :to="{ name: 'mail' }" />
      <h1 class="font-semibold text-highlighted">
        Filters
      </h1>
      <SettingsTabs class="ml-auto" />
    </header>

    <main class="mx-auto flex w-full max-w-2xl flex-1 flex-col gap-4 p-4 sm:p-6">
      <div v-if="loadError" class="flex flex-col items-center gap-3 pt-16 text-center">
        <UIcon name="i-lucide-cloud-off" class="size-10 text-dimmed" />
        <p class="text-sm text-muted">
          The filters could not be loaded.<br>{{ loadError }}
        </p>
        <UButton label="Try again" @click="load(true)" />
      </div>
      <div v-else-if="loading && scripts.length === 0" class="flex flex-col gap-3" aria-busy="true">
        <USkeleton v-for="n in 2" :key="n" class="h-20 w-full" />
      </div>
      <template v-else>
        <p class="text-sm text-muted">
          Rules look at mail as it arrives and file, mark, forward or delete it. They run in the order shown; a rule
          that moves or deletes a message is the last one that message meets.
        </p>

        <div class="flex flex-wrap items-center gap-2">
          <div class="flex gap-1" role="tablist" aria-label="How to show the filters">
            <UButton
              label="Rules" icon="i-lucide-list-checks" size="sm" color="neutral" :variant="tab === 'rules' ? 'soft' : 'ghost'"
              role="tab" :aria-selected="tab === 'rules'" data-testid="tab-rules" @click="tab = 'rules'"
            />
            <UButton
              label="Script" icon="i-lucide-file-code" size="sm" color="neutral" :variant="tab === 'script' ? 'soft' : 'ghost'"
              role="tab" :aria-selected="tab === 'script'" data-testid="tab-script" @click="tab = 'script'; draft = text"
            />
          </div>
          <USwitch
            v-if="current" class="ml-auto" :model-value="enabled" :disabled="busy" :label="enabled ? 'On' : 'Off'" data-testid="filters-enabled"
            @update:model-value="setEnabled($event === true)"
          />
        </div>

        <UAlert v-if="saveError" color="error" variant="subtle" icon="i-lucide-circle-alert" :title="saveError" role="alert" data-testid="filters-error" />
        <UAlert
          v-if="current && !enabled" color="warning" variant="subtle" icon="i-lucide-pause"
          title="The filters are off" description="They are kept, but new mail goes to the Inbox as it is."
        />

        <!-- Rules -->
        <template v-if="tab === 'rules'">
          <div v-if="rules === null" class="flex flex-col items-start gap-3 rounded-lg border border-default p-4" data-testid="custom-script">
            <p class="text-sm">
              This account's filter is a script that was written by hand, or by another program. It runs as it is; this
              list can only show rules the page made itself.
            </p>
            <div class="flex gap-2">
              <UButton label="Show the script" color="neutral" variant="subtle" size="sm" @click="tab = 'script'; draft = text" />
              <UButton label="Replace it with rules" color="neutral" variant="ghost" size="sm" :loading="busy" @click="startOver" />
            </div>
          </div>
          <template v-else>
            <ol v-if="rules.length" class="flex flex-col gap-3" aria-label="Rules">
              <li v-for="(r, i) in rules" :key="i" class="rounded-lg border border-default p-4" data-testid="rule">
                <div class="flex items-start gap-1">
                  <div class="min-w-0 flex-1">
                    <p class="truncate font-medium text-highlighted">
                      {{ r.name }}
                    </p>
                    <p class="text-sm text-muted">
                      If {{ describe(r).when }}:<br>
                      <span class="text-default">{{ describe(r).then }}</span>
                    </p>
                  </div>
                  <UButton icon="i-lucide-arrow-up" size="sm" color="neutral" variant="ghost" :disabled="i === 0 || busy" :aria-label="`Move ${r.name} up`" @click="moveRule(i, -1)" />
                  <UButton icon="i-lucide-arrow-down" size="sm" color="neutral" variant="ghost" :disabled="i === rules.length - 1 || busy" :aria-label="`Move ${r.name} down`" @click="moveRule(i, 1)" />
                  <UButton icon="i-lucide-pencil" size="sm" color="neutral" variant="ghost" :aria-label="`Edit ${r.name}`" @click="edit(i)" />
                  <UButton icon="i-lucide-trash-2" size="sm" color="neutral" variant="ghost" :disabled="busy" :aria-label="`Remove ${r.name}`" @click="removeRule(i)" />
                </div>
              </li>
            </ol>
            <p v-else class="rounded-lg border border-dashed border-default p-6 text-center text-sm text-muted">
              No rules yet. All mail arrives in the Inbox.
            </p>
            <div>
              <UButton label="Add a rule" icon="i-lucide-plus" data-testid="add-rule" :disabled="busy" @click="edit(-1)" />
            </div>
          </template>
        </template>

        <!-- The script as text -->
        <template v-else>
          <p class="text-sm text-muted">
            The filter as the server runs it: a Sieve script. What is saved here is checked by the server first. A script
            changed by hand is no longer shown as rules.
          </p>
          <UTextarea
            v-model="draft" :rows="18" class="w-full" :ui="{ base: 'font-mono text-sm' }" spellcheck="false" autocapitalize="off"
            aria-label="Sieve script" data-testid="script-editor"
          />
          <div class="flex items-center gap-2">
            <UButton label="Save" :loading="busy" :disabled="draft === text && !!current" data-testid="save-script" @click="saveScript" />
            <UButton label="Undo changes" color="neutral" variant="ghost" :disabled="draft === text" @click="draft = text" />
            <span v-if="current" class="ml-auto truncate text-xs text-dimmed">Script “{{ current.name }}”</span>
          </div>
        </template>
      </template>
    </main>

    <UModal v-model:open="open" :title="editing === -1 ? 'New rule' : 'Edit rule'" description="What to look for in a new message, and what to do with it.">
      <template #body>
        <form id="rule-form" class="flex flex-col gap-4" @submit.prevent="submitRule">
          <UAlert v-if="formError" color="error" variant="subtle" icon="i-lucide-circle-alert" :title="formError" role="alert" />
          <UFormField label="Name" name="name">
            <UInput v-model="form.name" name="name" maxlength="100" class="w-full" autofocus autocomplete="off" />
          </UFormField>

          <fieldset class="flex flex-col gap-2">
            <legend class="mb-1 text-sm font-medium">
              If
            </legend>
            <div v-for="(c, n) in form.conditions" :key="n" class="flex items-center gap-2" data-testid="condition">
              <USelect v-model="c.field" :items="fields" class="w-32 shrink-0" :aria-label="`Field of condition ${n + 1}`" />
              <span class="shrink-0 text-sm text-muted">contains</span>
              <UInput v-model="c.text" class="min-w-0 flex-1" autocomplete="off" :aria-label="`Text of condition ${n + 1}`" />
              <UButton
                v-if="form.conditions.length > 1" icon="i-lucide-x" size="sm" color="neutral" variant="ghost" :aria-label="`Remove condition ${n + 1}`"
                @click="form.conditions.splice(n, 1)"
              />
            </div>
            <div class="flex flex-wrap items-center gap-3">
              <UButton label="Another condition" icon="i-lucide-plus" size="sm" color="neutral" variant="ghost" @click="form.conditions.push({ field: 'subject', text: '' })" />
              <URadioGroup
                v-if="form.conditions.length > 1" v-model="form.match" orientation="horizontal" size="sm"
                :items="[{ label: 'All of these', value: 'all' }, { label: 'Any of these', value: 'any' }]"
              />
            </div>
          </fieldset>

          <fieldset class="flex flex-col gap-2">
            <legend class="mb-1 text-sm font-medium">
              Then
            </legend>
            <div class="flex items-center gap-2">
              <UCheckbox v-model="form.move" label="Move to" class="w-32 shrink-0" @update:model-value="$event === true && (form.remove = false)" />
              <USelect v-model="form.moveTo" :items="mailboxes" :disabled="!form.move" class="min-w-0 flex-1" aria-label="Mailbox to move to" placeholder="Choose a mailbox" />
            </div>
            <div class="flex items-center gap-2">
              <UCheckbox v-model="form.label" label="Label" class="w-32 shrink-0" />
              <USelect v-model="form.labelWith" :items="mailboxes" :disabled="!form.label" class="min-w-0 flex-1" aria-label="Mailbox to label with" placeholder="Choose a mailbox" />
            </div>
            <UCheckbox v-model="form.read" label="Mark as read" />
            <div class="flex items-center gap-2">
              <UCheckbox v-model="form.forward" label="Forward to" class="w-32 shrink-0" />
              <UInput v-model="form.forwardTo" :disabled="!form.forward" class="min-w-0 flex-1" inputmode="email" autocomplete="off" aria-label="Address to forward to" placeholder="someone@example.org" />
            </div>
            <UCheckbox v-model="form.remove" label="Delete it" @update:model-value="$event === true && (form.move = false)" />
          </fieldset>
        </form>
      </template>
      <template #footer>
        <div class="flex w-full justify-end gap-2">
          <UButton label="Cancel" color="neutral" variant="ghost" @click="editing = null" />
          <UButton type="submit" form="rule-form" :loading="busy" :label="editing === -1 ? 'Add' : 'Save'" />
        </div>
      </template>
    </UModal>
  </div>
</template>
