// SPDX-License-Identifier: AGPL-3.0-only

package nudge

import (
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// HTMLText is the new text of an HTML-only body, for Asks: the visible text
// with block boundaries as line breaks, and the QUOTED reply dropped — a
// <blockquote>, and the wrappers the common clients put around a quote
// (Gmail's gmail_quote, Thunderbird's moz-cite-prefix, Outlook's
// divRplyFwdMsg / appendonsend). ingest's htmlToText is a single-line preview
// extractor and keeps the quote, which would credit this message with
// questions the user asked earlier in the thread.
func HTMLText(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return ""
	}
	var b strings.Builder
	var walk func(n *html.Node) bool
	// walk returns false to stop the whole walk (Outlook's reply marker ends
	// the new text: everything after it in document order is the quote).
	walk = func(n *html.Node) bool {
		if n.Type == html.ElementNode {
			switch n.DataAtom {
			case atom.Script, atom.Style, atom.Head, atom.Title, atom.Noscript, atom.Blockquote:
				return true
			}
			for _, a := range n.Attr {
				v := strings.ToLower(a.Val)
				if a.Key == "class" && (strings.Contains(v, "gmail_quote") || strings.Contains(v, "moz-cite-prefix") || strings.Contains(v, "yahoo_quoted")) {
					return true
				}
				if a.Key == "id" && (strings.HasPrefix(v, "divrplyfwdmsg") || strings.HasPrefix(v, "appendonsend")) {
					return false
				}
			}
			switch n.DataAtom {
			case atom.P, atom.Div, atom.Br, atom.Hr, atom.Li, atom.Tr, atom.Table,
				atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6, atom.Pre:
				b.WriteByte('\n')
			}
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if !walk(c) {
				return false
			}
		}
		return true
	}
	walk(doc)
	return b.String()
}
