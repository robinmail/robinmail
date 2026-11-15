// The Go server of the development instance (contrib/dev/seed.sh), as in
// web/vite.config.ts. The browser's Host and Origin go through unchanged:
// the server compares the two, finds them equal (http://localhost:4200)
// and takes the request as its own page's. Its config needs no entry for
// this port.
const target = process.env.ROBINMAIL_DEV_URL ?? 'http://127.0.0.1:8080'
const to = { target, changeOrigin: false, secure: false }

export default { '/jmap': to, '/.well-known/jmap': to, '/auth': to, '/dav': to }
