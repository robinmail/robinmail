// Builds a robinmail binary with the Angular webmail in it, without
// touching the Go tree: the "webmail" build tag embeds internal/web/dist,
// and an overlay (go build -overlay) tells the compiler that this
// directory holds web-ng/dist/shell instead of whatever is there.
//
//   node scripts/go-build.mjs [output]      default: dist/robinmail-ng
//
// Until the server has a build tag of its own for this webmail, this is
// how a binary with it is made.
import { execFileSync } from 'node:child_process'
import { existsSync, readdirSync, statSync, writeFileSync } from 'node:fs'
import { join, resolve } from 'node:path'

const here = new URL('..', import.meta.url).pathname
const root = resolve(here, '..')
const shell = join(here, 'dist/shell')
const target = join(root, 'internal/web/dist')
const out = resolve(process.argv[2] ?? join(here, 'dist/robinmail-ng'))

function files(dir, base = '') {
  if (!existsSync(dir)) return []
  return readdirSync(dir).flatMap((name) => {
    const rel = join(base, name)
    return statSync(join(dir, name)).isDirectory() ? files(join(dir, name), rel) : [rel]
  })
}

const ours = files(shell)
if (!ours.includes('index.html')) throw new Error('web-ng/dist/shell has no index.html: run "npm run build" first')
const replace = {}
// What is in internal/web/dist now (the Vue build) is hidden ...
for (const f of files(target)) replace[join(target, f)] = ''
// ... and ours is put in its place.
for (const f of ours) replace[join(target, f)] = join(shell, f)
const overlay = join(here, 'dist/overlay.json')
writeFileSync(overlay, JSON.stringify({ Replace: replace }, null, 2))
execFileSync('go', ['build', '-tags', 'webmail', '-overlay', overlay, '-o', out, './cmd/robinmail'], { cwd: root, stdio: 'inherit' })
console.log(`${out}: robinmail with the Angular webmail (${ours.length} files)`)
