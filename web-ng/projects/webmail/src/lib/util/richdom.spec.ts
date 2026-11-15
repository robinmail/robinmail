import { describe, expect, it } from 'vitest'
import { exportHtml, importHtml, safeHref } from './richdom'

const noImage = () => null

function roundTrip(html: string, imageUrl: (cid: string) => string | null = noImage): string {
  const host = document.createElement('div')
  host.appendChild(importHtml(html, imageUrl))
  return exportHtml(host)
}

describe('the rich text editor and HTML', () => {
  it('keeps the few elements it knows', () => {
    expect(roundTrip('<p>Hello <b>Ann</b>, <em>see</em></p><ul><li>one</li></ul>')).toBe('<p>Hello <b>Ann</b>, <i>see</i></p><ul><li>one</li></ul>')
  })

  it('drops scripts, styles and frames with what is in them, and unwraps what it does not know', () => {
    expect(roundTrip('<p onclick="x()">a<script>alert(1)</script><span style="color:red">b</span><iframe src="//x"></iframe></p><style>p{}</style>')).toBe('<p>ab</p>')
  })

  it('keeps a link only with an address a message may carry', () => {
    expect(roundTrip('<a href="https://example.org/x">ok</a> <a href="javascript:alert(1)">bad</a>')).toBe('<a href="https://example.org/x">ok</a> bad')
    expect(safeHref('mailto:ann@example.org')).toBe('mailto:ann@example.org')
    expect(safeHref('data:text/html,x')).toBeNull()
  })

  it('shows an inline image from the server and writes it back as cid:', () => {
    const url = (cid: string) => (cid === 'pic@x' ? '/jmap/download/a/b/pic.png' : null)
    const host = document.createElement('div')
    host.appendChild(importHtml('<p><img src="cid:pic@x"><img src="https://tracker.example/p.gif"><img src="cid:other"></p>', url))
    expect([...host.querySelectorAll('img')].map((i) => i.getAttribute('src'))).toEqual(['/jmap/download/a/b/pic.png'])
    expect(exportHtml(host)).toBe('<p><img src="cid:pic@x" alt=""></p>')
  })

  it('writes a quote the way mail programs show one', () => {
    expect(roundTrip('<blockquote>said</blockquote>')).toBe('<blockquote style="margin:0 0 0 .8ex;border-left:1px solid #ccc;padding-left:1ex">said</blockquote>')
  })
})
