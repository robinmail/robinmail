// The Go server of the development instance (contrib/dev/seed.sh). The
// browser's Origin and Host go through unchanged, as with web/vite.config.ts;
// the instance's config has to allow the origin http://localhost:4200.
const target = process.env.ROBINMAIL_DEV_URL ?? 'http://127.0.0.1:8080'
const to = { target, changeOrigin: false, secure: false }

export default { '/jmap': to, '/.well-known/jmap': to, '/auth': to }
