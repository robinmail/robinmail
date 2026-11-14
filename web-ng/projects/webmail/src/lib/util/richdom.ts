// The rich text editor's two crossings between HTML as a string and the
// page: reading stored HTML into the editor and writing the editor out.
// Both go through the same short list of elements. HTML is never set as
// innerHTML: it is parsed into a document of its own, which runs nothing
// and loads nothing, and the nodes the list allows are made anew here.

import { escapeHtml } from '@robinmail/jmap'

// Elements that are kept, and what they are written as.
const keep: Record<string, string> = {
  P: 'p', DIV: 'div', BR: 'br', B: 'b', STRONG: 'b', I: 'i', EM: 'i', U: 'u', UL: 'ul', OL: 'ol', LI: 'li',
  BLOCKQUOTE: 'blockquote', A: 'a', IMG: 'img', PRE: 'pre',
}
// Elements that go with everything in them.
const drop = new Set(['SCRIPT', 'STYLE', 'HEAD', 'TITLE', 'TEMPLATE', 'SVG', 'MATH', 'NOSCRIPT', 'OBJECT', 'IFRAME', 'EMBED', 'FORM', 'LINK', 'META'])

// Elements a paragraph cannot hold.
const blocksInside = new Set(['UL', 'OL', 'BLOCKQUOTE', 'PRE', 'P', 'DIV'])

// What mail programs show a quote as; without it a blockquote is only indented.
const QUOTE_STYLE = 'margin:0 0 0 .8ex;border-left:1px solid #ccc;padding-left:1ex'

// safeHref lets through the links a message may carry.
export function safeHref(href: string | null): string | null {
  const h = (href ?? '').trim()
  return /^(https?:\/\/|mailto:)/i.test(h) ? h : null
}

// importHtml builds the editor's content from stored HTML. imageUrl gives
// the address to show an inline image at, by its Content-ID, or null when
// the draft has no such attachment; other images are left out.
export function importHtml(html: string, imageUrl: (cid: string) => string | null): DocumentFragment {
  const source = new DOMParser().parseFromString(html, 'text/html')
  const out = document.createDocumentFragment()
  const copy = (from: Node, to: Node) => {
    for (const n of from.childNodes) {
      if (n.nodeType === Node.TEXT_NODE) {
        to.appendChild(document.createTextNode(n.nodeValue ?? ''))
        continue
      }
      if (n.nodeType !== Node.ELEMENT_NODE) continue
      const el = n as Element
      const tag = el.tagName.toUpperCase()
      if (drop.has(tag)) continue
      const name = keep[tag]
      if (!name) {
        copy(el, to)
        continue
      }
      if (name === 'img') {
        const src = el.getAttribute('src') ?? ''
        const cid = /^cid:/i.test(src) ? src.slice(4) : ''
        const url = cid ? imageUrl(cid) : null
        if (url) to.appendChild(imageElement(cid, url))
        continue
      }
      const made = document.createElement(name)
      if (name === 'a') {
        const href = safeHref(el.getAttribute('href'))
        if (!href) {
          copy(el, to)
          continue
        }
        made.setAttribute('href', href)
      }
      to.appendChild(made)
      copy(el, made)
    }
  }
  copy(source.body, out)
  return out
}

export function imageElement(cid: string, url: string): HTMLImageElement {
  const img = document.createElement('img')
  img.src = url
  img.alt = ''
  img.dataset.cid = cid
  return img
}

// exportHtml writes the editor's content as the HTML that is stored and
// sent: only the listed elements, links with a checked address, inline
// images as "cid:".
export function exportHtml(root: Node): string {
  let out = ''
  const walk = (node: Node) => {
    for (const n of node.childNodes) {
      if (n.nodeType === Node.TEXT_NODE) {
        out += escapeHtml(n.nodeValue ?? '')
        continue
      }
      if (n.nodeType !== Node.ELEMENT_NODE) continue
      const el = n as HTMLElement
      const tag = el.tagName.toUpperCase()
      if (drop.has(tag)) continue
      const name = keep[tag]
      if (!name) {
        walk(el)
        continue
      }
      if (name === 'br') out += '<br>'
      else if (name === 'img') {
        if (el.dataset.cid) out += `<img src="cid:${escapeHtml(el.dataset.cid)}" alt="">`
      } else if (name === 'a') {
        const href = safeHref(el.getAttribute('href'))
        if (href) out += `<a href="${escapeHtml(href)}">`
        walk(el)
        if (href) out += '</a>'
      } else if (name === 'p' && [...el.children].some((c) => blocksInside.has(c.tagName.toUpperCase()))) {
        // The browser puts a list or a quote made on a line inside that
        // line's paragraph, where HTML does not allow one.
        walk(el)
      } else {
        out += name === 'blockquote' ? `<blockquote style="${QUOTE_STYLE}">` : `<${name}>`
        walk(el)
        out += `</${name}>`
      }
    }
  }
  walk(root)
  return out
}
