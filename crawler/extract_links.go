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
	atom.Video:  "src",
	atom.Audio:  "src",
	atom.Source: "src",
	atom.Iframe: "src",
}

func ExtractHTTPLinksFromHTML(doc *html.Node, rawBaseURL string) ([]*url.URL, error) {
	baseURL, err := url.Parse(rawBaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse url %s: %w", rawBaseURL, err)
	}

	var links []*url.URL

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
			links = append(links, fullLink)
			break
		}
	}
	return links, nil
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
