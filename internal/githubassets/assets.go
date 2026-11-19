// Package githubassets discovers and downloads GitHub issue attachments.
package githubassets

import (
	"fmt"
	"html"
	"net/url"
	"path"
	"regexp"
	"strings"
)

const MaxAssets = 32

// Asset preserves an attachment reference from the issue snapshot, not file bytes.
type Asset struct {
	URL    string `json:"url"`
	Name   string `json:"name"`
	Source string `json:"source,omitempty"`
}

// Source associates attachment references with an issue description or comment.
type Source struct{ Name, Body string }

var urls = regexp.MustCompile(`https://[^\s<>"'` + "`" + `]+`)

// Discover recognizes GitHub uploads in Markdown, HTML, and bare links. Ordinary
// web links are left in the prompt, not fetched by the harness.
func Discover(sources []Source) []Asset {
	var assets []Asset
	seen := map[string]bool{}
	for _, source := range sources {
		for _, raw := range urls.FindAllString(html.UnescapeString(source.Body), -1) {
			raw = strings.TrimRight(raw, ".,;!]}")
			// Remove Markdown's closing delimiter without losing balanced
			// parentheses inside an uploaded filename.
			for strings.HasSuffix(raw, ")") && strings.Count(raw, ")") > strings.Count(raw, "(") {
				raw = strings.TrimSuffix(raw, ")")
			}
			u, err := url.Parse(raw)
			if err != nil || !allowedAttachment(u) {
				continue
			}
			u.Fragment = ""
			raw = u.String()
			if seen[raw] {
				continue
			}
			seen[raw] = true
			assets = append(assets, Asset{URL: raw, Name: path.Base(u.Path), Source: source.Name})
		}
	}
	return assets
}

func Validate(assets []Asset) error {
	if len(assets) > MaxAssets {
		return fmt.Errorf("assets exceeds the %d attachment limit", MaxAssets)
	}
	for i, asset := range assets {
		u, err := url.Parse(asset.URL)
		if err != nil || !allowedAttachment(u) {
			return fmt.Errorf("assets[%d].url must be an HTTPS GitHub attachment URL", i)
		}
	}
	return nil
}

func safeURL(u *url.URL) bool {
	if u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Opaque != "" {
		return false
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return !strings.ContainsAny(u.Path, "\\\r\n")
}

func allowedAttachment(u *url.URL) bool {
	if !safeURL(u) {
		return false
	}
	switch u.Host {
	case "user-images.githubusercontent.com", "private-user-images.githubusercontent.com":
		return u.Path != "" && u.Path != "/"
	case "github.com":
		parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
		if len(parts) == 3 && parts[0] == "user-attachments" && parts[1] == "assets" && parts[2] != "" {
			return true
		}
		if len(parts) >= 4 && parts[0] == "user-attachments" && parts[1] == "files" && parts[2] != "" && parts[3] != "" {
			return true
		}
		return len(parts) >= 5 && parts[0] != "" && parts[1] != "" && (parts[2] == "assets" || parts[2] == "files") && parts[3] != "" && parts[4] != ""
	}
	return false
}
