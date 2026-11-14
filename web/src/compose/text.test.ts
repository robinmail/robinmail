import { describe, expect, it } from 'vitest'
import { decodeEntities, htmlToText, quote, textToHtml } from './text'

describe('htmlToText', () => {
  it('turns paragraphs and line breaks into lines', () => {
    expect(htmlToText('<p>Hello <b>Ann</b>,</p><p>One<br>Two</p>')).toBe('Hello Ann,\n\nOne\nTwo')
  })

  it('collapses the white space of the source', () => {
    expect(htmlToText('<div>  a\n   b  </div>\n<div>c</div>')).toBe('a b\nc')
  })

  it('leaves out scripts, styles and comments with their content', () => {
    expect(htmlToText('<style>p{color:red}</style><p>Text</p><script>alert("<p>x</p>")</script><!-- <p>no</p> -->')).toBe('Text')
  })

  it('quotes a blockquote, also a nested one', () => {
    expect(htmlToText('<p>Reply</p><blockquote><p>First</p><blockquote>Deeper</blockquote><p>Last</p></blockquote>')).toBe(
      'Reply\n\n> First\n>\n> > Deeper\n>\n> Last',
    )
  })

  it('marks list items', () => {
    expect(htmlToText('<ul><li>one</li><li>two</li></ul><ol><li>first</li><li>second</li></ol>')).toBe('- one\n- two\n\n1. first\n2. second')
  })

  it('keeps the address of a link unless it is what the link says', () => {
    expect(htmlToText('<a href="https://example.org/x">the page</a>')).toBe('the page <https://example.org/x>')
    expect(htmlToText('<a href="https://example.org/x">https://example.org/x</a>')).toBe('https://example.org/x')
    expect(htmlToText('<a href="mailto:ann@example.org">ann@example.org</a>')).toBe('ann@example.org')
    expect(htmlToText('<a href="javascript:alert(1)">click</a>')).toBe('click')
  })

  it('decodes entities and never leaves markup', () => {
    expect(htmlToText('<p>a &amp; b &lt;script&gt; &#8364; &#x1F600; &nbsp;c &eacute;</p>')).toBe('a & b <script> € 😀 c é')
    expect(htmlToText('<img src=x onerror="alert(1)" alt="A picture">')).toBe('[A picture]')
  })

  it('keeps the lines of preformatted text', () => {
    expect(htmlToText('<pre>a\n  b\n</pre><p>c</p>')).toBe('a\n  b\n\nc')
  })

  it('reads an unfinished document', () => {
    expect(htmlToText('<p>Hello <b>world')).toBe('Hello world')
    expect(htmlToText('1 < 2 and <3')).toBe('1 < 2 and <3')
  })
})

describe('decodeEntities', () => {
  it('leaves an unknown name and refuses bad code points', () => {
    expect(decodeEntities('&nosuch; &#0; &#xD800; &#x110000;')).toBe('&nosuch; � � �')
  })
})

describe('quote', () => {
  it('puts "> " before each line', () => {
    expect(quote('one\n\ntwo\n> three\n')).toBe('> one\n>\n> two\n>> three')
  })
})

describe('textToHtml', () => {
  it('escapes and makes paragraphs', () => {
    expect(textToHtml('a <b>\nc & d\n\nnext')).toBe('<p>a &lt;b&gt;<br>c &amp; d</p><p>next</p>')
  })

  it('makes a blockquote of quoted lines', () => {
    expect(textToHtml('Hi\n\n> one\n> > two\n> three')).toBe('<p>Hi</p><blockquote><p>one</p><blockquote><p>two</p></blockquote><p>three</p></blockquote>')
  })

  it('comes back as the same text', () => {
    const text = 'Hello,\n\nOne line\nanother line\n\n-- \nAnn\n\nOn Monday, Bob wrote:\n\n> first\n>\n> second'
    expect(htmlToText(textToHtml(text))).toBe(text)
    // A quote right under its line gets the blank line HTML shows there.
    expect(htmlToText(textToHtml('Bob wrote:\n> first'))).toBe('Bob wrote:\n\n> first')
  })
})
