// A search snippet (RFC 8621 5) is HTML in which only <mark> and </mark>
// are markup and everything else is escaped. It is not put into the page
// as HTML: it is cut into pieces of text, marked or not, and the page
// makes its own elements for them. A server that sent other markup would
// have it shown as the text it is.

export interface SnippetPart {
  text: string
  mark: boolean
}

const entities: Record<string, string> = { '&lt;': '<', '&gt;': '>', '&amp;': '&', '&quot;': '"', '&#34;': '"', '&#39;': "'" }

function unescape(s: string): string {
  return s.replace(/&(?:lt|gt|amp|quot|#34|#39);/g, (m) => entities[m] ?? m)
}

export function snippetParts(html: string): SnippetPart[] {
  const out: SnippetPart[] = []
  let mark = false
  for (const piece of html.split(/(<mark>|<\/mark>)/)) {
    if (piece === '<mark>') mark = true
    else if (piece === '</mark>') mark = false
    else if (piece) out.push({ text: unescape(piece), mark })
  }
  return out
}
