import { describe, expect, it } from 'vitest'
import { snippetParts } from './snippet'

describe('snippetParts', () => {
  it('cuts at the marks and unescapes the rest', () => {
    expect(snippetParts('The <mark>harbour fee</mark> is &lt;due&gt; &amp; late')).toEqual([
      { text: 'The ', mark: false },
      { text: 'harbour fee', mark: true },
      { text: ' is <due> & late', mark: false },
    ])
  })
  it('keeps other markup as text', () => {
    expect(snippetParts('<b>x</b><mark><img src=x onerror=alert(1)></mark>')).toEqual([
      { text: '<b>x</b>', mark: false },
      { text: '<img src=x onerror=alert(1)>', mark: true },
    ])
  })
  it('does not unescape twice', () => {
    expect(snippetParts('&amp;lt;')).toEqual([{ text: '&lt;', mark: false }])
  })
  it('is empty for an empty snippet', () => {
    expect(snippetParts('')).toEqual([])
  })
})
