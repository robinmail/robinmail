<script setup lang="ts">
// Password and security: the password, app passwords for mail programs,
// two-step login, and the browsers that are logged in.
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { auth, compose, store } from '../app'
import * as api from '../auth/api'
import { fullDate, shortDate } from '../lib/format'

const router = useRouter()
const toast = useToast()

const session = ref<api.Session | null>(null)
const appPasswords = ref<api.AppPassword[]>([])
const passwordOpensMail = ref(true)
const totp = ref({ enabled: false, recoveryCodes: 0 })
const sessions = ref<api.WebSession[]>([])
const mailSettings = ref<api.MailSettings | null>(null)
const loaded = ref(false)
const loadError = ref('')

// gone handles an answer that says the login ended: back to the login page.
function gone(e: unknown): boolean {
  if (!(e instanceof api.AuthError) || e.status !== 401 || e.message === 'wrong code') return false
  auth.account = null
  compose.clear()
  store.reset()
  void router.replace({ name: 'login', query: { next: '/settings/security' } })
  return true
}

async function load() {
  loadError.value = ''
  try {
    const [who, apps, t, s] = await Promise.all([api.currentSession(), api.appPasswords(), api.totpState(), api.sessions()])
    if (!who.session) throw new api.AuthError(401, 'not logged in')
    session.value = who.session
    appPasswords.value = apps.list
    passwordOpensMail.value = apps.passwordOpensMail
    totp.value = t
    sessions.value = s
    loaded.value = true
    // What a mail program needs. The page works without it.
    mailSettings.value = await api.mailSettings().catch(() => null)
  } catch (e) {
    if (!gone(e)) loadError.value = api.explain(e)
  }
}
onMounted(() => {
  document.title = 'Security · robinmail'
  void load()
})

const hasPassword = computed(() => session.value?.hasPassword ?? true)
const viaProvider = computed(() => session.value?.method.startsWith('oidc:') ?? false)
const host = window.location.hostname

// One dialog at a time. Each has its own little form.
type Dialog =
  | { kind: 'new-app' }
  | { kind: 'app-secret'; app: api.AppPassword }
  | { kind: 'revoke-app'; app: api.AppPassword }
  | { kind: 'totp-password' }
  | { kind: 'totp-scan'; enrolment: api.Enrolment }
  | { kind: 'codes'; codes: string[]; fresh: boolean }
  | { kind: 'totp-off' }
  | { kind: 'new-codes' }
  | { kind: 'revoke-session'; session: api.WebSession }
  | { kind: 'revoke-others' }
const dialog = ref<Dialog | null>(null)
const open = computed({ get: () => dialog.value !== null, set: (v) => { if (!v) close() } })
const busy = ref(false)
const error = ref('')
const form = ref({ name: '', imap: true, smtp: true, jmap: true, password: '', code: '' })

function show(d: Dialog) {
  dialog.value = d
  error.value = ''
  busy.value = false
  form.value = { name: '', imap: true, smtp: true, jmap: true, password: '', code: '' }
}
function close() {
  dialog.value = null
  form.value.password = ''
  form.value.code = ''
}

const titles: Record<Dialog['kind'], string> = {
  'new-app': 'New app password',
  'app-secret': 'Your new app password',
  'revoke-app': 'Revoke this app password?',
  'totp-password': 'Turn on two-step login',
  'totp-scan': 'Scan the code',
  'codes': 'Recovery codes',
  'totp-off': 'Turn off two-step login',
  'new-codes': 'New recovery codes',
  'revoke-session': 'Log this browser out?',
  'revoke-others': 'Log out everywhere else?',
}

// run does what a dialog's form asks and shows what went wrong in it.
async function run(work: () => Promise<void>) {
  if (busy.value) return
  busy.value = true
  error.value = ''
  try {
    await work()
  } catch (e) {
    if (!gone(e)) error.value = api.explain(e)
  } finally {
    busy.value = false
  }
}

async function submit() {
  const d = dialog.value
  if (!d) return
  const f = form.value
  switch (d.kind) {
    case 'new-app':
      return run(async () => {
        const all = f.imap && f.smtp && f.jmap
        const scopes = all ? [] : [f.imap && 'imap', f.smtp && 'smtp', f.jmap && 'jmap'].filter((s): s is string => !!s)
        if (!all && scopes.length === 0) throw new api.AuthError(400, 'choose at least one kind of program')
        const app = await api.createAppPassword(f.name.trim(), scopes, f.password)
        await load()
        show({ kind: 'app-secret', app })
      })
    case 'revoke-app':
      return run(async () => {
        await api.revokeAppPassword(d.app.id)
        await load()
        close()
        toast.add({ title: `Revoked “${d.app.name}”`, description: 'Mail programs that used it are logged out.', icon: 'i-lucide-check' })
      })
    case 'totp-password':
      return run(async () => {
        const enrolment = await api.totpEnrol(f.password)
        show({ kind: 'totp-scan', enrolment })
      })
    case 'totp-scan':
      return run(async () => {
        const codes = await api.totpConfirm(f.code.replace(/\s/g, ''))
        await load()
        show({ kind: 'codes', codes, fresh: true })
      })
    case 'totp-off':
      return run(async () => {
        await api.totpDisable(f.password, f.code)
        await load()
        close()
        toast.add({ title: 'Two-step login is off', icon: 'i-lucide-check' })
      })
    case 'new-codes':
      return run(async () => {
        const codes = await api.newRecoveryCodes(f.password, f.code)
        await load()
        show({ kind: 'codes', codes, fresh: false })
      })
    case 'revoke-session':
      return run(async () => {
        await api.revokeSession(d.session.id)
        if (d.session.current) throw new api.AuthError(401, 'logged out')
        await load()
        close()
      })
    case 'revoke-others':
      return run(async () => {
        const n = await api.revokeOtherSessions()
        await load()
        close()
        toast.add({ title: n === 1 ? 'Logged out 1 other browser' : `Logged out ${n} other browsers`, icon: 'i-lucide-check' })
      })
    default:
      close()
  }
}

// The password form is on the page itself.
const pw = ref({ current: '', next: '', again: '' })
const pwBusy = ref(false)
const pwError = ref('')
async function changePassword() {
  if (pwBusy.value) return
  pwError.value = ''
  if (pw.value.next !== pw.value.again) {
    pwError.value = 'The two new passwords are not the same.'
    return
  }
  pwBusy.value = true
  try {
    await api.changePassword(pw.value.current, pw.value.next)
    pw.value = { current: '', next: '', again: '' }
    await load()
    toast.add({ title: 'Password changed', description: 'Every other browser was logged out.', icon: 'i-lucide-check' })
  } catch (e) {
    if (!gone(e)) pwError.value = api.explain(e)
  } finally {
    pwBusy.value = false
  }
}

async function copy(text: string) {
  try {
    await navigator.clipboard.writeText(text)
    toast.add({ title: 'Copied', icon: 'i-lucide-clipboard-check', duration: 1500 })
  } catch {
    toast.add({ title: 'Could not copy: select the text and copy it yourself', color: 'warning' })
  }
}

const scopeNames: Record<string, string> = { imap: 'reading (IMAP)', smtp: 'sending (SMTP)', jmap: 'JMAP' }
function scopeText(a: api.AppPassword): string {
  return a.scopes.length ? a.scopes.map((s) => scopeNames[s] ?? s).join(', ') : 'reading, sending and JMAP'
}
function methodText(m: string): string {
  const [first, second] = m.split('+')
  const how = first!.startsWith('oidc:') ? `through ${first!.slice(5)}` : 'with the password'
  return second === 'totp' ? `${how} and a code` : second === 'recovery' ? `${how} and a recovery code` : how
}
</script>

<template>
  <div class="flex min-h-full flex-col bg-default">
    <header class="flex h-(--ui-header-height) shrink-0 items-center gap-2 border-b border-default px-3 sm:px-6">
      <UButton icon="i-lucide-arrow-left" color="neutral" variant="ghost" aria-label="Back to the mail" :to="{ name: 'mail' }" />
      <h1 class="font-semibold text-highlighted">
        Security
      </h1>
      <SettingsTabs class="ml-auto" />
    </header>

    <main class="mx-auto flex w-full max-w-2xl flex-1 flex-col gap-8 p-4 sm:p-6">
      <div v-if="loadError" class="flex flex-col items-center gap-3 pt-16 text-center">
        <UIcon name="i-lucide-cloud-off" class="size-10 text-dimmed" />
        <p class="text-sm text-muted">
          {{ loadError }}
        </p>
        <UButton label="Try again" @click="load" />
      </div>
      <div v-else-if="!loaded" class="flex flex-col gap-3" aria-busy="true">
        <USkeleton v-for="n in 4" :key="n" class="h-28 w-full" />
      </div>
      <template v-else>
        <!-- Password -->
        <section aria-labelledby="h-password" class="flex flex-col gap-3" data-testid="section-password">
          <h2 id="h-password" class="text-base font-semibold text-highlighted">
            Password
          </h2>
          <p v-if="!hasPassword" class="text-sm text-muted">
            This account has no password: you log in through your provider. To use a mail program, make an app password below.
          </p>
          <form v-else class="flex flex-col gap-3" data-testid="password-form" @submit.prevent="changePassword">
            <p class="text-sm text-muted">
              The password of your account opens the webmail. Changing it logs every other browser out.
            </p>
            <UAlert v-if="pwError" color="error" variant="subtle" icon="i-lucide-circle-alert" :title="pwError" role="alert" />
            <UFormField label="Current password" name="current">
              <UInput v-model="pw.current" type="password" name="current" autocomplete="current-password" required class="w-full" />
            </UFormField>
            <UFormField label="New password" name="new" help="At least 8 characters.">
              <UInput v-model="pw.next" type="password" name="new" autocomplete="new-password" minlength="8" required class="w-full" />
            </UFormField>
            <UFormField label="New password, again" name="again">
              <UInput v-model="pw.again" type="password" name="again" autocomplete="new-password" minlength="8" required class="w-full" />
            </UFormField>
            <div>
              <UButton type="submit" :loading="pwBusy" label="Change password" />
            </div>
          </form>
        </section>

        <!-- App passwords -->
        <section aria-labelledby="h-apps" class="flex flex-col gap-3" data-testid="section-apps">
          <h2 id="h-apps" class="text-base font-semibold text-highlighted">
            App passwords
          </h2>
          <p class="text-sm text-muted">
            A mail program such as Thunderbird or the Mail app on a phone cannot do two-step login or log in through a provider.
            It gets a password of its own instead: one per device, made here, which you can take away again without touching the others.
          </p>
          <UAlert
            v-if="passwordOpensMail && hasPassword" color="neutral" variant="subtle" icon="i-lucide-info" data-testid="password-opens-mail"
            title="Your account password still works in mail programs."
            description="It stops working there once you make an app password or turn on two-step login. From then on it only opens the webmail."
          />
          <UAlert
            v-else color="neutral" variant="subtle" icon="i-lucide-lock" data-testid="password-closed-for-mail"
            title="Mail programs need an app password."
            description="Your account password only opens the webmail."
          />
          <ul v-if="appPasswords.length" class="flex flex-col gap-2" aria-label="App passwords">
            <li v-for="a in appPasswords" :key="a.id" class="flex items-start gap-3 rounded-lg border border-default p-3" data-testid="app-password">
              <UIcon name="i-lucide-key-round" class="mt-0.5 size-5 shrink-0 text-dimmed" />
              <div class="min-w-0 flex-1 text-sm">
                <p class="truncate font-medium text-highlighted">
                  {{ a.name }}
                </p>
                <p class="text-muted">
                  For {{ scopeText(a) }}. Made {{ shortDate(a.createdAt) }}.
                </p>
                <p class="text-muted" :title="a.usedAt ? fullDate(a.usedAt) : undefined">
                  {{ a.usedAt ? `Last used ${shortDate(a.usedAt)} from ${a.usedIp}` : 'Never used' }}
                </p>
              </div>
              <UButton label="Revoke" size="sm" color="error" variant="ghost" :aria-label="`Revoke ${a.name}`" @click="show({ kind: 'revoke-app', app: a })" />
            </li>
          </ul>
          <div>
            <UButton label="New app password" icon="i-lucide-plus" color="neutral" variant="subtle" data-testid="new-app-password" @click="show({ kind: 'new-app' })" />
          </div>
        </section>

        <!-- Setting up a mail program -->
        <section
          v-if="mailSettings && (mailSettings.imap.length || mailSettings.smtp.length)"
          aria-labelledby="h-mail-apps" class="flex flex-col gap-3" data-testid="section-mail-apps"
        >
          <h2 id="h-mail-apps" class="text-base font-semibold text-highlighted">
            Set up a mail program
          </h2>
          <p class="text-sm text-muted">
            Thunderbird and many other programs find these settings themselves from your address.
            Log in with your address and {{ passwordOpensMail && hasPassword ? 'your password or an app password' : 'an app password' }}.
          </p>
          <dl class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 rounded-lg border border-default p-3 text-sm" data-testid="mail-settings">
            <dt class="text-muted">
              User name
            </dt>
            <dd class="select-all break-all font-mono text-highlighted">
              {{ mailSettings.username }}
            </dd>
            <template v-for="m in mailSettings.imap" :key="`imap-${m.port}`">
              <dt class="text-muted">
                Incoming (IMAP)
              </dt>
              <dd class="break-all font-mono text-highlighted" data-testid="mail-settings-imap">
                {{ m.host }}, port {{ m.port }}, {{ m.security }}
              </dd>
            </template>
            <template v-for="m in mailSettings.smtp" :key="`smtp-${m.port}`">
              <dt class="text-muted">
                Outgoing (SMTP)
              </dt>
              <dd class="break-all font-mono text-highlighted" data-testid="mail-settings-smtp">
                {{ m.host }}, port {{ m.port }}, {{ m.security }}
              </dd>
            </template>
          </dl>
          <div v-if="mailSettings.profile" class="flex flex-col gap-2">
            <div>
              <UButton
                to="/auth/mobileconfig" external download="mail.mobileconfig" label="Profile for iPhone, iPad and Mac"
                icon="i-lucide-download" color="neutral" variant="subtle" data-testid="mobileconfig"
              />
            </div>
            <p class="text-sm text-muted">
              The profile adds this account to the Mail app. It is not signed, so the device shows it as “Unsigned”, and it holds no password: the device asks for one.
            </p>
          </div>
        </section>

        <!-- Two-step login -->
        <section aria-labelledby="h-totp" class="flex flex-col gap-3" data-testid="section-totp">
          <h2 id="h-totp" class="text-base font-semibold text-highlighted">
            Two-step login
          </h2>
          <template v-if="!totp.enabled">
            <p class="text-sm text-muted">
              With two-step login the webmail asks for your password and then for a code from an authenticator app on your phone.
              Somebody who learns your password cannot get in without the phone.
            </p>
            <p v-if="viaProvider" class="text-sm text-muted">
              When you log in through your provider, the provider decides about a second step, not robinmail.
            </p>
            <div>
              <UButton label="Turn on two-step login" icon="i-lucide-smartphone" data-testid="totp-on" @click="show({ kind: 'totp-password' })" />
            </div>
          </template>
          <template v-else>
            <UAlert
              color="success" variant="subtle" icon="i-lucide-shield-check" title="Two-step login is on." data-testid="totp-enabled"
              :description="`${totp.recoveryCodes} of 10 recovery codes are left.`"
            />
            <div class="flex flex-wrap gap-2">
              <UButton label="New recovery codes" color="neutral" variant="subtle" data-testid="new-codes" @click="show({ kind: 'new-codes' })" />
              <UButton label="Turn off" color="error" variant="ghost" data-testid="totp-off" @click="show({ kind: 'totp-off' })" />
            </div>
          </template>
        </section>

        <!-- Sessions -->
        <section aria-labelledby="h-sessions" class="flex flex-col gap-3" data-testid="section-sessions">
          <h2 id="h-sessions" class="text-base font-semibold text-highlighted">
            Where you are logged in
          </h2>
          <p class="text-sm text-muted">
            Browsers that are logged in to the webmail. A browser is logged out by itself when it was not used for a while.
          </p>
          <ul class="flex flex-col gap-2" aria-label="Sessions">
            <li v-for="s in sessions" :key="s.id" class="flex items-start gap-3 rounded-lg border border-default p-3" data-testid="session">
              <UIcon :name="/iPhone|Android/.test(s.device) ? 'i-lucide-smartphone' : 'i-lucide-monitor'" class="mt-0.5 size-5 shrink-0 text-dimmed" />
              <div class="min-w-0 flex-1 text-sm">
                <p class="flex items-center gap-2 font-medium text-highlighted">
                  <span class="truncate" :title="s.agent">{{ s.device }}</span>
                  <UBadge v-if="s.current" label="This browser" color="primary" variant="subtle" size="sm" />
                </p>
                <p class="text-muted" :title="fullDate(s.createdAt)">
                  Logged in {{ shortDate(s.createdAt) }} {{ methodText(s.method) }}, from {{ s.ip }}
                </p>
                <p class="text-muted" :title="fullDate(s.seenAt)">
                  Last seen {{ shortDate(s.seenAt) }}<template v-if="s.seenIp !== s.ip">
                    from {{ s.seenIp }}
                  </template>
                </p>
              </div>
              <UButton
                label="Log out" size="sm" color="neutral" variant="ghost" :aria-label="`Log out ${s.device}${s.current ? ' (this browser)' : ''}`"
                @click="show({ kind: 'revoke-session', session: s })"
              />
            </li>
          </ul>
          <div v-if="sessions.length > 1">
            <UButton label="Log out everywhere else" color="neutral" variant="subtle" data-testid="revoke-others" @click="show({ kind: 'revoke-others' })" />
          </div>
        </section>
      </template>
    </main>

    <UModal v-model:open="open" :title="dialog ? titles[dialog.kind] : ''" :dismissible="dialog?.kind !== 'app-secret' && dialog?.kind !== 'codes'">
      <template #body>
        <form id="security-form" class="flex flex-col gap-4" @submit.prevent="submit">
          <UAlert v-if="error" color="error" variant="subtle" icon="i-lucide-circle-alert" :title="error" role="alert" />

          <template v-if="dialog?.kind === 'new-app'">
            <UFormField label="Name" name="name" help="What it is for, so you know which one to revoke later: “Phone”, “Thunderbird at work”.">
              <UInput v-model="form.name" name="name" maxlength="100" required autofocus autocomplete="off" class="w-full" />
            </UFormField>
            <fieldset class="flex flex-col gap-2">
              <legend class="mb-1 text-sm font-medium text-default">
                It may be used for
              </legend>
              <UCheckbox v-model="form.imap" name="imap" label="Reading mail (IMAP)" />
              <UCheckbox v-model="form.smtp" name="smtp" label="Sending mail (SMTP)" />
              <UCheckbox v-model="form.jmap" name="jmap" label="JMAP programs" />
            </fieldset>
            <UFormField v-if="hasPassword" label="Your account password" name="password" help="To make sure it is you.">
              <UInput v-model="form.password" type="password" name="password" autocomplete="current-password" required class="w-full" />
            </UFormField>
          </template>

          <template v-else-if="dialog?.kind === 'app-secret'">
            <p class="text-sm">
              This is the password for <span class="font-medium">{{ dialog.app.name }}</span>. Put it into the mail program now:
              it is shown this once and cannot be looked up later.
            </p>
            <div class="flex items-center gap-2 rounded-lg bg-elevated p-3">
              <code class="min-w-0 flex-1 select-all break-all font-mono text-sm text-highlighted" data-testid="app-secret">{{ dialog.app.secret }}</code>
              <UButton icon="i-lucide-copy" color="neutral" variant="ghost" aria-label="Copy the app password" @click="copy(dialog.app.secret ?? '')" />
            </div>
            <dl class="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-sm">
              <dt class="text-dimmed">
                User name
              </dt>
              <dd class="break-all">
                {{ session?.username }}
              </dd>
              <dt class="text-dimmed">
                Incoming (IMAP)
              </dt>
              <dd>{{ host }}, port 993, TLS</dd>
              <dt class="text-dimmed">
                Outgoing (SMTP)
              </dt>
              <dd>{{ host }}, port 465, TLS</dd>
            </dl>
            <p class="text-sm text-muted">
              The dashes are there for reading; with or without them it is the same password.
            </p>
          </template>

          <p v-else-if="dialog?.kind === 'revoke-app'" class="text-sm">
            The program that uses <span class="font-medium">{{ dialog.app.name }}</span> is logged out at once and cannot log in again with it.
            Your other app passwords keep working.
          </p>

          <template v-else-if="dialog?.kind === 'totp-password'">
            <p class="text-sm">
              You need an authenticator app on your phone, such as Aegis, 2FAS, FreeOTP or the one of your password manager.
            </p>
            <p class="text-sm text-muted">
              Once it is on, your account password no longer works in mail programs: they need an app password.
            </p>
            <UFormField v-if="hasPassword" label="Your account password" name="password" help="To make sure it is you.">
              <UInput v-model="form.password" type="password" name="password" autocomplete="current-password" required autofocus class="w-full" />
            </UFormField>
          </template>

          <template v-else-if="dialog?.kind === 'totp-scan'">
            <p class="text-sm">
              Scan this with the authenticator app, then type the six digits it shows.
            </p>
            <!-- Dark on white whatever the theme: that is what cameras read. -->
            <div v-if="dialog.enrolment.qr?.length" class="self-center rounded-lg bg-white p-2">
              <svg
                :viewBox="`-4 -4 ${dialog.enrolment.qr.length + 8} ${dialog.enrolment.qr.length + 8}`" class="size-52" role="img"
                aria-label="QR code with the secret for the authenticator app" shape-rendering="crispEdges" data-testid="totp-qr"
              >
                <path :d="api.qrPath(dialog.enrolment.qr)" fill="#000" />
              </svg>
            </div>
            <details class="text-sm">
              <summary class="cursor-pointer text-muted">
                Cannot scan it? Type this key into the app instead
              </summary>
              <div class="mt-2 flex items-center gap-2 rounded-lg bg-elevated p-3">
                <code class="min-w-0 flex-1 select-all break-all font-mono text-sm" data-testid="totp-secret">{{ dialog.enrolment.secret }}</code>
                <UButton icon="i-lucide-copy" color="neutral" variant="ghost" aria-label="Copy the key" @click="copy(dialog.enrolment.secret)" />
              </div>
              <p class="mt-1 text-muted">
                Time-based, 6 digits, 30 seconds, SHA-1: what an app uses unless told otherwise.
              </p>
            </details>
            <UFormField label="Code" name="code">
              <UInput v-model="form.code" name="code" inputmode="numeric" autocomplete="one-time-code" maxlength="9" required autofocus placeholder="123456" class="w-full" :ui="{ base: 'tracking-widest' }" />
            </UFormField>
          </template>

          <template v-else-if="dialog?.kind === 'codes'">
            <p class="text-sm">
              <template v-if="dialog.fresh">
                Two-step login is on.
              </template>
              Keep these recovery codes somewhere safe, not on the phone. Each one logs you in once when you do not have the phone.
              They are shown this once<template v-if="!dialog.fresh">
                ; the old ones no longer work
              </template>.
            </p>
            <ul class="grid grid-cols-2 gap-x-4 gap-y-1 rounded-lg bg-elevated p-3 font-mono text-sm" data-testid="recovery-codes">
              <li v-for="c in dialog.codes" :key="c" class="select-all">
                {{ c }}
              </li>
            </ul>
            <div>
              <UButton icon="i-lucide-copy" label="Copy all" color="neutral" variant="subtle" size="sm" @click="copy(dialog.codes.join('\n'))" />
            </div>
          </template>

          <template v-else-if="dialog?.kind === 'totp-off' || dialog?.kind === 'new-codes'">
            <p class="text-sm">
              {{ dialog.kind === 'totp-off'
                ? 'After this the webmail asks for your password only. The authenticator app and the recovery codes stop working.'
                : 'You get ten new recovery codes. The ones you have stop working.' }}
            </p>
            <UFormField v-if="hasPassword" label="Your account password" name="password">
              <UInput v-model="form.password" type="password" name="password" autocomplete="current-password" required autofocus class="w-full" />
            </UFormField>
            <UFormField label="Code" name="code" help="From the authenticator app, or a recovery code.">
              <UInput v-model="form.code" name="code" autocomplete="one-time-code" autocapitalize="off" spellcheck="false" maxlength="40" required class="w-full" />
            </UFormField>
          </template>

          <p v-else-if="dialog?.kind === 'revoke-session'" class="text-sm">
            <template v-if="dialog.session.current">
              This is the browser you are using: you will be logged out here.
            </template>
            <template v-else>
              {{ dialog.session.device }}, last seen {{ shortDate(dialog.session.seenAt) }} from {{ dialog.session.seenIp }}, will have to log in again.
            </template>
          </p>

          <p v-else-if="dialog?.kind === 'revoke-others'" class="text-sm">
            Every browser but this one will have to log in again. Mail programs with an app password are not affected.
          </p>
        </form>
      </template>
      <template #footer>
        <div class="flex w-full justify-end gap-2">
          <UButton v-if="dialog?.kind === 'app-secret' || dialog?.kind === 'codes'" label="I have saved it" data-testid="dialog-done" @click="close" />
          <template v-else>
            <UButton label="Cancel" color="neutral" variant="ghost" @click="close" />
            <UButton
              type="submit" form="security-form" :loading="busy" data-testid="dialog-submit"
              :color="dialog?.kind === 'revoke-app' || dialog?.kind === 'totp-off' ? 'error' : 'primary'"
              :label="dialog?.kind === 'new-app' ? 'Make the password' : dialog?.kind === 'revoke-app' ? 'Revoke'
                : dialog?.kind === 'totp-password' ? 'Continue' : dialog?.kind === 'totp-scan' ? 'Turn on'
                  : dialog?.kind === 'totp-off' ? 'Turn off' : dialog?.kind === 'new-codes' ? 'Make new codes' : 'Log out'"
            />
          </template>
        </div>
      </template>
    </UModal>
  </div>
</template>
