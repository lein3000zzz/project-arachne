package parser

import (
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var noscriptContext = &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}

// ScanText is what the regex extractors see for an HTML page: the document as the
// HTML parser decoded it - text, attribute values, comments, script bodies - one
// per line. Over the raw markup they take "&amp;" and "&quot;" for part of a URL.
func ScanText(root *html.Node) string {
	var b strings.Builder

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode, html.CommentNode:
			b.WriteString(n.Data)
			b.WriteByte('\n')
		case html.ElementNode:
			for _, attr := range n.Attr {
				b.WriteString(attr.Val)
				b.WriteByte('\n')
			}

			// Parsed with scripting on, as goquery does, <noscript> holds its markup as
			// raw text - entities and all. A crawler that runs no scripts sees it as HTML.
			if n.DataAtom == atom.Noscript && n.FirstChild != nil && n.FirstChild.Type == html.TextNode {
				if nodes, err := html.ParseFragment(strings.NewReader(n.FirstChild.Data), noscriptContext); err == nil {
					for _, node := range nodes {
						walk(node)
					}

					return
				}
			}
		}

		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}

	walk(root)

	return b.String()
}
