package crawler

import (
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var LinkAttrByNode = map[atom.Atom]string{
	atom.A:      "href",
	atom.Link:   "href",
	atom.Script: "src",
	atom.Img:    "src",
	// atom.Video:  "src",
	// atom.Audio:  "src",
	// atom.Source: "src",
	// atom.Iframe: "src",
}

type LinkData struct {
	link      *url.URL
	assetType string
}

func ExtractHTTPLinksFromHTML(doc *html.Node, rawBaseURL string) ([]LinkData, error) {
	baseURL, err := url.Parse(rawBaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse url %s: %w", rawBaseURL, err)
	}

	var linksData []LinkData

	uniqueLinks := map[string]struct{}{
		normalizeAbsURL(baseURL).String(): {},
	}
	baseFound := false
	for n := range doc.Descendants() {
		if baseFound {
			break
		}
		if n.Type != html.ElementNode {
			continue
		}
		if n.DataAtom == atom.Base {
			for _, a := range n.Attr {
				if a.Key != "href" {
					continue
				}
				baseFound = true
				baseURL = resolveURL(a.Val, baseURL)
				break
			}
		}
	}

	for n := range doc.Descendants() {
		if n.Type != html.ElementNode {
			continue
		}
		attr, ok := LinkAttrByNode[n.DataAtom]
		if !ok {
			continue
		}

		if n.DataAtom == atom.Link {
			var rel string
			for _, a := range n.Attr {
				if a.Key == "rel" {
					rel = a.Val
					break
				}
			}
			if rel != "stylesheet" && rel != "alternate" {
				// NB: attempt to fix automated tests (included favicon?)
				continue
			}
		}
		for _, a := range n.Attr {
			if a.Key != attr {
				continue
			}
			fullLink := resolveURL(a.Val, baseURL)
			if !isValidWebURL(fullLink) {
				break
			}
			fullLink.Fragment = ""
			fullLink.RawFragment = ""
			dedupLink := normalizeAbsURL(fullLink).String()
			if _, ok := uniqueLinks[dedupLink]; ok {
				break
			}
			uniqueLinks[dedupLink] = struct{}{}

			linksData = append(linksData, LinkData{
				link:      fullLink,
				assetType: getAssetType(n),
			})
			break
		}
	}
	return linksData, nil
}

func normalizeAbsURL(webURL *url.URL) *url.URL {
	if webURL.Path == "/" {
		webURL.Path = ""
		return webURL
	}
	return webURL
}

func resolveURL(pathStr string, abs *url.URL) *url.URL {
	trimmed := strings.TrimSpace(pathStr)
	if trimmed == "" {
		return abs
	}
	ref, err := url.Parse(trimmed)
	if err != nil {
		return abs
	}
	resolved := abs.ResolveReference(ref)
	return resolved
}

func getAssetType(n *html.Node) string {
	switch n.Data {
	case "script":
		return "script"
	case "img":
		return "image"
	case "link":
		for _, a := range n.Attr {
			if a.Key == "rel" && a.Val == "stylesheet" {
				return "style"
			}
		}
	}
	return ""
}
