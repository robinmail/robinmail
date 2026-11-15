// After "ng build shell": gives dist/shell the layout the Go server serves
// the Vue webmail in. index.html and the files with fixed names stay at
// the top; everything named by its content (scripts, styles, media) goes
// to assets/, which the server sends with a year of caching. index.html
// already points there: the build's deployUrl is /assets/.
import { mkdirSync, readdirSync, renameSync, statSync } from 'node:fs'
import { join } from 'node:path'

const dist = new URL('../dist/shell/', import.meta.url).pathname
const keep = new Set(['index.html', 'favicon.svg', '3rdpartylicenses.txt', 'assets'])
mkdirSync(join(dist, 'assets'), { recursive: true })
let moved = 0
for (const name of readdirSync(dist)) {
  if (keep.has(name) || statSync(join(dist, name)).isDirectory()) continue
  // Only what carries a hash in its name may be cached forever.
  if (!/-[A-Za-z0-9_-]{8}\.[a-z0-9]+$/.test(name)) throw new Error(`dist/shell/${name} has no content hash in its name: it cannot go to assets/`)
  renameSync(join(dist, name), join(dist, 'assets', name))
  moved++
}
console.log(`dist/shell: ${moved} files moved to assets/`)
