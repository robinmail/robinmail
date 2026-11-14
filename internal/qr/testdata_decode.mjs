// Reads QR codes with jsQR, a decoder that shares no code with
// internal/qr. Input on stdin: JSON, a list of codes, each a list of rows
// of "0" and "1". Output: JSON, the bytes each code holds as hex, or null.
// Used by TestAgainstDecoder; run from web/, where jsqr is installed.
import { createRequire } from 'node:module'
const require = createRequire(process.cwd() + '/')
const jsQR = require('jsqr')

let input = ''
for await (const chunk of process.stdin) input += chunk
const scale = 4, quiet = 4
const out = JSON.parse(input).map((rows) => {
  const n = (rows.length + 2 * quiet) * scale
  const px = new Uint8ClampedArray(n * n * 4).fill(255)
  rows.forEach((row, r) => [...row].forEach((bit, c) => {
    if (bit !== '1') return
    for (let y = 0; y < scale; y++) for (let x = 0; x < scale; x++) {
      const i = (((r + quiet) * scale + y) * n + (c + quiet) * scale + x) * 4
      px[i] = px[i + 1] = px[i + 2] = 0
    }
  }))
  const got = jsQR(px, n, n)
  return got ? Buffer.from(got.binaryData).toString('hex') : null
})
process.stdout.write(JSON.stringify(out))
