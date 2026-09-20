package crawler

import (
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

type SEO struct {
	HasTitle       bool   `json:"has_title"`
	Title          string `json:"title"`
	HasDescription bool   `json:"has_description"`
	Description    string `json:"description"`
	HasH1          bool   `json:"has_h1"`
}

func AnalyzeSEO(doc *html.Node) SEO {
	seo := SEO{}
	for n := range doc.Descendants() {
		if seo.HasTitle && seo.HasDescription && seo.HasH1 {
			break
		}
		if n.Type != html.ElementNode {
			continue
		}
		if n.DataAtom == atom.Title && !seo.HasTitle {
			seo.HasTitle = true

			var sb strings.Builder
			for n := range n.ChildNodes() {
				if n.Type == html.TextNode {
					sb.WriteString(n.Data)
				}
			}
			seo.Title = extractNormalizedText(sb.String())
		}
		if n.DataAtom == atom.Meta && !seo.HasDescription {
			var name, content string

			for _, a := range n.Attr {
				switch a.Key {
				case "name":
					name = a.Val
				case "content":
					content = a.Val
				}
			}
			if strings.EqualFold(name, "description") {
				seo.HasDescription = true
				seo.Description = extractNormalizedText(content)
			}
		}
		if n.DataAtom == atom.H1 && !seo.HasH1 {
			seo.HasH1 = true
		}
	}
	return seo
}

func extractNormalizedText(s string) string {
	parts := strings.Fields(s)
	return strings.Join(parts, " ")
}
