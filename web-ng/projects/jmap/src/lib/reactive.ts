// reactive() gives the stores state that Angular follows: a deep proxy over
// plain objects and arrays. Every read through it reads one signal and
// every write bumps that signal, so a template or computed() that read any
// of the state runs again after any change to it. That is coarser than a
// signal per field, and it keeps the stores plain TypeScript: they assign
// to their state as they would to any object.

import { signal } from '@angular/core'

const version = signal(0)
const proxies = new WeakMap<object, object>()
const raws = new WeakMap<object, object>()

function plain(v: unknown): v is object {
  if (v === null || typeof v !== 'object') return false
  const p = Object.getPrototypeOf(v)
  return p === Object.prototype || p === Array.prototype || p === null
}

// toRaw returns the object behind a proxy of reactive().
export function toRaw<T>(v: T): T {
  return (typeof v === 'object' && v !== null && (raws.get(v) as T | undefined)) || v
}

// stateVersion reads the signal: a computed() over data that is reached
// some other way than through the proxy can depend on the state with it.
export function stateVersion(): number {
  return version()
}

// Looking an object up in a reactive array finds it whether the caller
// holds the object itself or its proxy.
const searches = {
  indexOf(this: unknown[], x: unknown, from?: number): number {
    const raw = toRaw(x)
    return toRaw(this).findIndex((v, i) => i >= (from ?? 0) && toRaw(v) === raw)
  },
  lastIndexOf(this: unknown[], x: unknown): number {
    const raw = toRaw(x)
    return toRaw(this).findLastIndex((v) => toRaw(v) === raw)
  },
  includes(this: unknown[], x: unknown): boolean {
    const raw = toRaw(x)
    return toRaw(this).some((v) => toRaw(v) === raw || (raw !== raw && v !== v))
  },
}

export function reactive<T extends object>(target: T): T {
  if (raws.has(target)) return target
  const known = proxies.get(target)
  if (known) return known as T
  const p = new Proxy(target, {
    get(t, k, r) {
      version()
      if (Array.isArray(t) && typeof k === 'string' && k in searches) return searches[k as keyof typeof searches]
      const v = Reflect.get(t, k, r)
      return plain(v) ? reactive(v) : v
    },
    has(t, k) {
      version()
      return Reflect.has(t, k)
    },
    ownKeys(t) {
      version()
      return Reflect.ownKeys(t)
    },
    set(t, k, v, r) {
      const next = toRaw(v)
      const had = Object.prototype.hasOwnProperty.call(t, k)
      const old = (t as Record<PropertyKey, unknown>)[k]
      const ok = Reflect.set(t, k, next, r)
      if (!had || !Object.is(old, next)) version.update((n) => n + 1)
      return ok
    },
    deleteProperty(t, k) {
      const had = Object.prototype.hasOwnProperty.call(t, k)
      const ok = Reflect.deleteProperty(t, k)
      if (had) version.update((n) => n + 1)
      return ok
    },
  })
  proxies.set(target, p)
  raws.set(p, target)
  return p as T
}
