// Writes dev/robinmail.e2e.json: the development config of seed.sh plus the
// OpenID providers the browser tests log in through.
//
//   test       the fake provider of contrib/dev/oidc-provider on
//              127.0.0.1:8090, which e2e.sh starts. Always there.
//   keycloak   the Keycloak of contrib/compose/keycloak.yaml on
//              127.0.0.1:8180, only when it answers right now. Without it
//              the Keycloak test skips itself.
//
// The secrets here are those of that fake provider and of that realm
// file: test values for a process and a container on loopback.
import { readFileSync, writeFileSync } from 'node:fs'

const keycloak = 'http://127.0.0.1:8180/realms/robinmail'
const cfg = JSON.parse(readFileSync('dev/robinmail.json', 'utf8'))
// ROBINMAIL_E2E_PORT moves the instance off the development ports, for
// when a development server is already running there: the web listener
// goes to that port, SMTP to the one after it, and the IMAP and
// submission listeners to the four from ten after it (8090 is the fake
// provider's).
const port = Number(process.env.ROBINMAIL_E2E_PORT ?? 8080)
if (port !== 8080) {
  cfg.web = { ...cfg.web, http_listen: `127.0.0.1:${port}` }
  cfg.smtp = { ...cfg.smtp, listen: `127.0.0.1:${port + 1}` }
  // The browser tests do not connect to these; the page that shows a
  // user the settings of a mail program reads them from the config.
  cfg.imap = { ...cfg.imap, listen_tls: `127.0.0.1:${port + 10}`, listen: `127.0.0.1:${port + 11}` }
  cfg.submission = { ...cfg.submission, listen_tls: `127.0.0.1:${port + 12}`, listen: `127.0.0.1:${port + 13}` }
}
const oidc = [
  { name: 'test', label: 'Test provider', issuer: 'http://127.0.0.1:8090', client_id: 'robinmail', client_secret: 'test-secret-of-the-fake-provider' },
]
try {
  const res = await fetch(`${keycloak}/.well-known/openid-configuration`, { signal: AbortSignal.timeout(2000) })
  // The realm's client knows the redirect URI on ports 8080 and 8085.
  if ((port === 8080 || port === 8085) && res.ok && (await res.json()).issuer === keycloak) {
    oidc.push({ name: 'keycloak', label: 'Keycloak', issuer: keycloak, client_id: 'robinmail', client_secret: 'robinmail-dev-keycloak-client-secret' })
  }
} catch {
  // not running
}
cfg.auth = { ...(cfg.auth ?? {}), oidc }
writeFileSync('dev/robinmail.e2e.json', JSON.stringify(cfg, null, 2) + '\n', { mode: 0o600 })
console.log(`dev/robinmail.e2e.json: providers ${oidc.map((p) => p.name).join(', ')}`)
