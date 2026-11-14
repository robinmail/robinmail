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
const oidc = [
  { name: 'test', label: 'Test provider', issuer: 'http://127.0.0.1:8090', client_id: 'robinmail', client_secret: 'test-secret-of-the-fake-provider' },
]
try {
  const res = await fetch(`${keycloak}/.well-known/openid-configuration`, { signal: AbortSignal.timeout(2000) })
  if (res.ok && (await res.json()).issuer === keycloak) {
    oidc.push({ name: 'keycloak', label: 'Keycloak', issuer: keycloak, client_id: 'robinmail', client_secret: 'robinmail-dev-keycloak-client-secret' })
  }
} catch {
  // not running
}
cfg.auth = { ...(cfg.auth ?? {}), oidc }
writeFileSync('dev/robinmail.e2e.json', JSON.stringify(cfg, null, 2) + '\n', { mode: 0o600 })
console.log(`dev/robinmail.e2e.json: providers ${oidc.map((p) => p.name).join(', ')}`)
