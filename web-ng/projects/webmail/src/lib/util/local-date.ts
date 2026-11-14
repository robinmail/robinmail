// Dates between JMAP (UTC, "2026-12-24T08:00:00Z") and the value of an
// <input type="datetime-local"> ("2026-12-24T09:00", in the browser's own
// time zone, no seconds).

const two = (n: number) => String(n).padStart(2, '0')

// toLocalInput turns a UTCDate into the input's value; '' for null.
export function toLocalInput(utc: string | null): string {
  if (!utc) return ''
  const d = new Date(utc)
  if (Number.isNaN(d.getTime())) return ''
  return `${d.getFullYear()}-${two(d.getMonth() + 1)}-${two(d.getDate())}T${two(d.getHours())}:${two(d.getMinutes())}`
}

// fromLocalInput turns the input's value into a UTCDate; null for ''.
export function fromLocalInput(local: string): string | null {
  if (!local) return null
  const d = new Date(local)
  if (Number.isNaN(d.getTime())) return null
  return d.toISOString().replace(/\.\d{3}Z$/, 'Z')
}
