// Text and HTML as strings. Nothing here touches the DOM: the HTML of a
// message is read as text and never becomes part of the page.

const named: Record<string, string> = {
  amp: '&', lt: '<', gt: '>', quot: '"', apos: "'", nbsp: ' ', copy: '©', reg: '®', trade: '™', hellip: '…',
  mdash: '—', ndash: '–', lsquo: '‘', rsquo: '’', ldquo: '“', rdquo: '”', laquo: '«', raquo: '»', bull: '•',
  middot: '·', euro: '€', pound: '£', yen: '¥', cent: '¢', sect: '§', deg: '°', times: '×', eacute: 'é',
  egrave: 'è', ecirc: 'ê', euml: 'ë', aacute: 'á', agrave: 'à', acirc: 'â', auml: 'ä', aring: 'å', ccedil: 'ç',
  iacute: 'í', iuml: 'ï', oacute: 'ó', ouml: 'ö', ocirc: 'ô', uacute: 'ú', uuml: 'ü', ntilde: 'ñ', szlig: 'ß',
  zwnj: '', zwj: '', shy: '',
}

export function decodeEntities(s: string): string {
  return s.replace(/&(#x[0-9a-f]+|#[0-9]+|[a-z][a-z0-9]*);?/gi, (all, ref: string) => {
    if (ref[0] === '#') {
      const n = ref[1] === 'x' || ref[1] === 'X' ? parseInt(ref.slice(2), 16) : parseInt(ref.slice(1), 10)
      if (!Number.isFinite(n) || n === 0 || n > 0x10ffff || (n >= 0xd800 && n <= 0xdfff)) return '�'
      return String.fromCodePoint(n)
    }
    return named[ref] ?? named[ref.toLowerCase()] ?? all
  })
}

export function escapeHtml(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;')
}

const skipped = new Set(['script', 'style', 'head', 'title', 'template', 'svg', 'math', 'noscript', 'object', 'iframe'])
const blocks = new Set([
  'p', 'div', 'section', 'article', 'header', 'footer', 'table', 'tr', 'ul', 'ol', 'li', 'h1', 'h2', 'h3', 'h4', 'h5',
  'h6', 'pre', 'blockquote', 'hr', 'address', 'dl', 'dt', 'dd', 'figure', 'figcaption', 'center',
])
const spaced = new Set(['p', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'pre', 'blockquote', 'ul', 'ol', 'table', 'hr'])
// The signature separator (RFC 3676 4.3) keeps its trailing space.
const SIGNATURE = '-- '

function attribute(tag: string, name: string): string {
  const m = new RegExp(`\\s${name}\\s*=\\s*(?:"([^"]*)"|'([^']*)'|([^\\s>]+))`, 'i').exec(tag)
  return m ? decodeEntities(m[1] ?? m[2] ?? m[3] ?? '') : ''
}

// htmlToText renders HTML as plain text: block elements become lines,
// a blockquote is quoted with "> ", list items get a mark, a link keeps
// its address, and scripts, styles and the like are left out with their
// content. It is a reading of the markup, not a browser: what it gives is
// what a reply quotes and what the text alternative of a rich message is.
export function htmlToText(html: string): string {
  const lines: string[] = []
  let line = ''
  let quote = 0
  let pre = 0
  let skip = ''
  // A blank line is owed before the next text, at this quote depth: the
  // lowest the text passed through since.
  let gap = false
  let gapQuote = 0
  // The mark of a list item, written with its first text.
  let mark = ''
  const lists: { ordered: boolean; n: number }[] = []
  const links: string[] = []

  const prefix = (depth = quote) => '> '.repeat(depth)
  const flush = () => {
    const text = pre || line === SIGNATURE ? line : line.replace(/[ \t]+$/g, '')
    if (text !== '' || pre) lines.push(text === SIGNATURE ? prefix() + text : (prefix() + text).replace(/\s+$/, ''))
    line = ''
  }
  const breakLine = (blank: boolean) => {
    if (line !== '') flush()
    if (blank && lines.length > 0 && !gap) {
      gap = true
      gapQuote = quote
    }
  }
  const write = (text: string) => {
    if (text === '') return
    if (line === '') {
      if (!pre) text = text.replace(/^[ \t]+/, '')
      if (text === '') return
      if (gap && lines.length > 0) lines.push(prefix(Math.min(gapQuote, quote)).trimEnd())
      gap = false
      line = mark
      mark = ''
    }
    line += text
  }

  const re = /<!--[\s\S]*?(?:-->|$)|<!\[CDATA\[[\s\S]*?\]\]>|<(\/?)([a-zA-Z][a-zA-Z0-9]*)\b(?:"[^"]*"|'[^']*'|[^'">])*>|<|[^<]+/g
  for (let m = re.exec(html); m; m = re.exec(html)) {
    const tag = m[2]?.toLowerCase()
    if (skip) {
      if (tag === skip && m[1] === '/') skip = ''
      continue
    }
    if (tag === undefined) {
      if (m[0].startsWith('<!')) continue
      const text = decodeEntities(m[0])
      if (pre) {
        const parts = text.split(/\r?\n/)
        parts.forEach((p, i) => {
          if (i > 0) {
            flush()
            if (line === '' && p === '' && i === parts.length - 1) return
          }
          write(p)
        })
      } else write(text.replace(/[ \t\r\n\f]+/g, ' '))
      continue
    }
    const closing = m[1] === '/'
    if (skipped.has(tag)) {
      if (!closing && !m[0].endsWith('/>')) skip = tag
      continue
    }
    if (tag === 'br') {
      if (line === '') lines.push(prefix().trimEnd())
      else flush()
      continue
    }
    if (tag === 'a') {
      if (!closing) links.push(attribute(m[0], 'href'))
      else {
        const href = links.pop() ?? ''
        const shown = line.trim()
        if (/^(https?:|mailto:)/i.test(href) && !shown.includes(href.replace(/^mailto:/i, '')) && href !== shown) write(` <${href}>`)
      }
      continue
    }
    if (tag === 'img') {
      const alt = attribute(m[0], 'alt')
      if (alt) write(`[${alt}]`)
      continue
    }
    if (tag === 'td' || tag === 'th') {
      if (closing) write(' ')
      continue
    }
    if (!blocks.has(tag)) continue
    breakLine(spaced.has(tag))
    if (tag === 'blockquote') {
      quote = Math.max(0, quote + (closing ? -1 : 1))
      gapQuote = Math.min(gapQuote, quote)
    }
    else if (tag === 'pre') pre = Math.max(0, pre + (closing ? -1 : 1))
    else if (tag === 'ul' || tag === 'ol') {
      if (closing) lists.pop()
      else lists.push({ ordered: tag === 'ol', n: 0 })
    } else if (tag === 'li' && !closing) {
      const l = lists.at(-1)
      mark = '  '.repeat(Math.max(0, lists.length - 1)) + (l?.ordered ? `${++l.n}. ` : '- ')
    } else if (tag === 'hr' && !closing) {
      write('----------')
      breakLine(true)
    }
  }
  if (line.trim() !== '') flush()
  return lines.join('\n').replace(/\n{3,}/g, '\n\n').replace(/^\n+|\s+$/g, '')
}

// quote puts "> " in front of every line (RFC 3676 style, without the
// space for a line that is quoted already).
export function quote(text: string): string {
  return text.replace(/\s+$/, '').split('\n').map((l) => (l.startsWith('>') ? `>${l}` : l === '' ? '>' : `> ${l}`)).join('\n')
}

// textToHtml writes plain text as the HTML the rich editor works on: a
// paragraph per block of lines, quoted lines as a blockquote.
export function textToHtml(text: string): string {
  const out: string[] = []
  const lines = text.replace(/\r\n/g, '\n').split('\n')
  let i = 0
  const block = (ls: string[]) => ls.map(escapeHtml).join('<br>')
  while (i < lines.length) {
    if (lines[i]!.startsWith('>')) {
      const q: string[] = []
      while (i < lines.length && lines[i]!.startsWith('>')) q.push(lines[i++]!.replace(/^> ?/, ''))
      out.push(`<blockquote>${textToHtml(q.join('\n'))}</blockquote>`)
      continue
    }
    if (lines[i] === '') {
      // An empty line between paragraphs is their margin; more are kept.
      let n = 0
      while (i < lines.length && lines[i] === '') {
        n++
        i++
      }
      const extra = out.length === 0 || i >= lines.length ? n : n - 1
      for (let k = 0; k < extra; k++) out.push('<p><br></p>')
      continue
    }
    const p: string[] = []
    while (i < lines.length && lines[i] !== '' && !lines[i]!.startsWith('>')) p.push(lines[i++]!)
    out.push(`<p>${block(p)}</p>`)
  }
  return out.join('')
}
