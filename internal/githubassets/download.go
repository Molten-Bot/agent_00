package githubassets

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

const MaxFileBytes int64 = 25 << 20
const MaxTotalBytes int64 = 100 << 20

type File struct {
	Asset
	Path      string `json:"path"`
	MediaType string `json:"mediaType"`
}

func (f File) IsImage() bool {
	switch f.MediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// AuthToken reads the active GitHub CLI credential without forwarding command output to logs.
func AuthToken(ctx context.Context, runner execx.Runner) string {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := runner.Run(ctx, execx.Command{Name: "gh", Args: []string{"auth", "token", "--hostname", "github.com"}})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(result.Stdout)
}

// Download saves bounded attachments outside repository checkouts. transport is
// injectable for tests; nil uses the standard HTTP transport. Credentials are
// sent only to github.com and never forwarded to signed storage redirects.
func Download(ctx context.Context, assets []Asset, dir, token string, transport http.RoundTripper) ([]File, error) {
	if err := Validate(assets); err != nil {
		return nil, err
	}
	if len(assets) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("attachment directory is required")
	}
	client := downloadClient(token, transport)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create attachment directory: %w", err)
	}
	var files []File
	var total int64
	for i, asset := range assets {
		file, size, err := downloadOne(ctx, client, asset, dir, token, i, min(MaxFileBytes, MaxTotalBytes-total))
		if err != nil {
			return nil, fmt.Errorf("download assets[%d]: %w", i, err)
		}
		total += size
		files = append(files, file)
	}
	return files, nil
}

// Preview fetches public attachment bytes for the UI, which cannot authorize viewers for private GitHub assets.
func Preview(ctx context.Context, asset Asset, transport http.RoundTripper) ([]byte, string, error) {
	u, err := url.Parse(asset.URL)
	if err != nil || !allowedAttachment(u) {
		return nil, "", fmt.Errorf("invalid attachment URL")
	}
	if u.Host == "private-user-images.githubusercontent.com" || u.RawQuery != "" {
		return nil, "", fmt.Errorf("attachment URL may require private access")
	}
	data, headers, err := fetch(ctx, downloadClient("", transport), asset.URL, "", MaxFileBytes)
	if err != nil {
		return nil, "", err
	}
	detected := http.DetectContentType(data)
	contentType := headers.Get("Content-Type")
	declared, _, _ := mime.ParseMediaType(contentType)
	if strings.HasPrefix(strings.ToLower(contentType), "text/html") ||
		strings.HasPrefix(detected, "text/html") && declared != "text/plain" {
		return nil, "", fmt.Errorf("GitHub returned HTML instead of an attachment; verify repository access")
	}
	switch detected {
	case "application/pdf", "image/bmp", "image/gif", "image/jpeg", "image/png", "image/webp", "image/x-icon", "video/mp4", "video/webm":
		return data, detected, nil
	case "application/ogg":
		return data, "video/ogg", nil
	}
	// Some browser media formats have no signature in http.DetectContentType.
	switch declared {
	case "image/avif", "image/svg+xml", "image/vnd.microsoft.icon", "video/ogg", "video/quicktime", "video/x-m4v":
		return data, declared, nil
	}
	if utf8.Valid(data) && bytes.IndexFunc(data, func(r rune) bool {
		return unicode.IsControl(r) && r != '\t' && r != '\n' && r != '\r'
	}) == -1 {
		return data, "text/plain; charset=utf-8", nil
	}
	return nil, "", fmt.Errorf("attachment format is not supported for previews")
}

func downloadClient(token string, transport http.RoundTripper) *http.Client {
	return &http.Client{Transport: transport, Timeout: 60 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many attachment redirects")
		}
		if !allowedDownload(req.URL) {
			return fmt.Errorf("attachment redirected outside GitHub storage")
		}
		req.Header.Del("Authorization")
		if req.URL.Host == "github.com" && token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return nil
	}}
}

func allowedDownload(u *url.URL) bool {
	if !safeURL(u) {
		return false
	}
	if allowedAttachment(u) {
		return true
	}
	switch u.Host {
	case "objects.githubusercontent.com", "github-production-user-asset-6210df.s3.amazonaws.com":
		return true
	}
	return false
}

func downloadOne(ctx context.Context, client *http.Client, asset Asset, dir, token string, index int, limit int64) (File, int64, error) {
	data, headers, err := fetch(ctx, client, asset.URL, token, limit)
	if err != nil {
		return File{}, 0, err
	}
	mediaType := http.DetectContentType(data)
	if strings.HasPrefix(mediaType, "text/html") || strings.HasPrefix(strings.ToLower(headers.Get("Content-Type")), "text/html") {
		return File{}, 0, fmt.Errorf("GitHub returned HTML instead of an attachment; verify repository access")
	}
	name := asset.Name
	if _, params, err := mime.ParseMediaType(headers.Get("Content-Disposition")); err == nil && params["filename"] != "" {
		name = params["filename"]
	}
	name = safeFilename(name)
	if filepath.Ext(name) == "" {
		switch mediaType {
		case "image/png":
			name += ".png"
		case "image/jpeg":
			name += ".jpg"
		case "image/gif":
			name += ".gif"
		case "image/webp":
			name += ".webp"
		case "application/pdf":
			name += ".pdf"
		}
	}
	filePath := filepath.Join(dir, fmt.Sprintf("%02d-%s", index+1, name))
	if err := os.WriteFile(filePath, data, 0o600); err != nil {
		return File{}, 0, fmt.Errorf("write attachment: %w", err)
	}
	return File{Asset: asset, Path: filePath, MediaType: mediaType}, int64(len(data)), nil
}

func fetch(ctx context.Context, client *http.Client, rawURL, token string, limit int64) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid attachment request")
	}
	if req.URL.Host == "github.com" && token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := client.Do(req)
	if err != nil {
		// net/http errors contain URLs, including signed redirect credentials.
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, fmt.Errorf("attachment request failed (network, timeout, or disallowed redirect)")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("GitHub returned HTTP %d; verify attachment availability and repository access", resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return nil, nil, fmt.Errorf("attachment exceeds download size limit")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read attachment response failed")
	}
	if int64(len(data)) > limit {
		return nil, nil, fmt.Errorf("attachment exceeds download size limit")
	}
	if len(data) == 0 {
		return nil, nil, fmt.Errorf("attachment is empty")
	}
	return data, resp.Header, nil
}

func safeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, name)
	name = strings.Trim(name, ".-")
	if len(name) > 120 {
		name = name[:120]
	}
	if name == "" {
		return "attachment"
	}
	return name
}
