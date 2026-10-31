// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"bytes"
	"html"
	"net/url"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	ghtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// MARKDOWN DRAFT BODIES (#872, ADR-045 §4).
//
// A model writes Markdown whether or not it is asked to, and a recipient's
// client shows **this** literally. So a draft tool's body is documented as
// Markdown and sent as multipart/alternative: the text/plain part is the body
// EXACTLY as the key wrote it, and the text/html part is this rendering.
//
// The renderer is goldmark (CommonMark, plus GFM tables, strikethrough and
// autolinks), configured so that what an untrusted caller writes cannot become
// active HTML:
//
//   - RAW HTML IS NEVER PASSED THROUGH. goldmark is not given WithUnsafe, and
//     raw HTML — inline or block — is rendered as ESCAPED TEXT by rawAsText
//     (goldmark's own default would drop it, losing "Hi <Bob>" from the HTML
//     part); a <script> arrives as the visible characters "<script>".
//   - LINKS go only to http, https (with a host) or mailto. Any other
//     destination — javascript:, data:, vbscript:, file:, a relative path, an
//     obfuscated scheme — loses its link and keeps its text (safeLinks).
//   - IMAGES ARE DROPPED and rendered as their alt text: a key cannot attach
//     files, and a remote image in a sent message is a tracking pixel the
//     owner never chose.
//   - A single newline is a line break (WithHardWraps): a model's line breaks
//     are meant, and a body with no Markdown at all reads as it was written.
//
// The API sanitises the result again with the send-path policy before it is
// stored (api.SanitizeOutboundHTML, the trust boundary for every htmlBody), so
// this configuration is the first of two layers, not the only one.
var markdown = goldmark.New(
	goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.Linkify),
	goldmark.WithParserOptions(parser.WithASTTransformers(util.Prioritized(safeLinks{}, 100))),
	goldmark.WithRendererOptions(
		ghtml.WithHardWraps(),
		renderer.WithNodeRenderers(util.Prioritized(rawAsText{}, 100)),
	),
)

// renderMarkdown is body as HTML, or "" when there is nothing to render (an
// empty or whitespace-only body stays a plain-text message).
func renderMarkdown(body string) string {
	if strings.TrimSpace(body) == "" {
		return ""
	}
	var b bytes.Buffer
	if err := markdown.Convert([]byte(body), &b); err != nil {
		// Fail to plain text: the text/plain part alone is the whole message.
		return ""
	}
	return b.String()
}

// allowedLink reports whether a link destination may stay a link.
func allowedLink(dest string) bool {
	u, err := url.Parse(strings.TrimSpace(dest))
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return u.Host != ""
	case "mailto":
		return u.Opaque != ""
	}
	return false
}

// safeLinks unwraps every link whose destination is not allowed (keeping its
// text) and replaces every image with its alt text.
type safeLinks struct{}

func (safeLinks) Transform(doc *ast.Document, reader text.Reader, _ parser.Context) {
	source := reader.Source()
	var unwrap []ast.Node
	var asText []*ast.AutoLink
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *ast.Image:
			unwrap = append(unwrap, v)
		case *ast.Link:
			// The parser keeps the destination RAW (goldmark v1.8.6
			// parser/link.go parseLinkDestination); the HTML renderer resolves
			// backslash escapes and entities only when it writes the href
			// (renderer/html/html.go renderLink -> util.URLEscape(dest, true)).
			// So the link must pass as written AND as it will be resolved: a
			// scheme spelled with an entity fails the first, and nothing can
			// slip past the second.
			if !allowedLink(string(v.Destination)) || !allowedLink(string(util.URLEscape(v.Destination, true))) {
				unwrap = append(unwrap, v)
			}
		case *ast.AutoLink:
			// An email autolink is rendered as mailto:; every other one must
			// carry an allowed scheme itself.
			if v.AutoLinkType != ast.AutoLinkEmail && !allowedLink(string(v.URL(source))) {
				asText = append(asText, v)
			}
		}
		return ast.WalkContinue, nil
	})
	for _, n := range unwrap {
		parent := n.Parent()
		if parent == nil {
			continue
		}
		for c := n.FirstChild(); c != nil; {
			next := c.NextSibling()
			parent.InsertBefore(parent, n, c)
			c = next
		}
		parent.RemoveChild(parent, n)
	}
	for _, n := range asText {
		if parent := n.Parent(); parent != nil {
			parent.ReplaceChild(parent, n, ast.NewString(n.Label(source)))
		}
	}
}

// rawAsText renders raw HTML as the escaped characters that were written.
type rawAsText struct{}

func (rawAsText) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindRawHTML, renderRawInline)
	reg.Register(ast.KindHTMLBlock, renderRawBlock)
}

func renderRawInline(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkSkipChildren, nil
	}
	segs := n.(*ast.RawHTML).Segments
	for i := range segs.Len() {
		s := segs.At(i)
		_, _ = w.WriteString(escapeLines(string(s.Value(source))))
	}
	return ast.WalkSkipChildren, nil
}

func renderRawBlock(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	b := n.(*ast.HTMLBlock)
	var raw strings.Builder
	lines := b.Lines()
	for i := range lines.Len() {
		l := lines.At(i)
		raw.Write(l.Value(source))
	}
	if b.HasClosure() {
		raw.Write(b.ClosureLine.Value(source))
	}
	_, _ = w.WriteString("<p>" + escapeLines(strings.TrimRight(raw.String(), "\r\n")) + "</p>\n")
	return ast.WalkSkipChildren, nil
}

// escapeLines is s escaped for HTML text, its line breaks kept as <br>.
func escapeLines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	parts := strings.Split(s, "\n")
	for i, p := range parts {
		parts[i] = html.EscapeString(p)
	}
	return strings.Join(parts, "<br>\n")
}
