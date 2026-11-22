package githubassets

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
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
const maxVideoPreviewBytes int64 = 100 << 20

// ErrPreviewAccess means GitHub denied access or concealed the attachment.
var ErrPreviewAccess = errors.New("GitHub attachment requires access")

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

// Preview opens an attachment using only the requesting viewer's optional
// GitHub token. The caller must close the returned body. Video streams can
// report a size limit or upstream read error after delivering partial data.
// Their bodies expose ContentLength() int64, which returns -1 if unknown.
func Preview(ctx context.Context, asset Asset, viewerToken string, transport http.RoundTripper) (io.ReadCloser, string, error) {
	u, err := url.Parse(asset.URL)
	if err != nil || !allowedAttachment(u) {
		return nil, "", fmt.Errorf("invalid attachment URL")
	}
	if u.Host == "private-user-images.githubusercontent.com" || u.RawQuery != "" {
		return nil, "", fmt.Errorf("attachment URL may require private access")
	}
	if u.Host != "github.com" {
		viewerToken = ""
	}
	client := downloadClient(viewerToken, transport)
	if u.Host == "github.com" {
		checkRedirect := client.CheckRedirect
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme == "https" && req.URL.Host == "github.com" && req.URL.Path == "/login" {
				return ErrPreviewAccess
			}
			return checkRedirect(req, via)
		}
	}
	resp, err := fetch(ctx, client, asset.URL, viewerToken, "")
	if err != nil {
		var status *attachmentHTTPError
		if u.Host == "github.com" && errors.As(err, &status) && status.githubAccess {
			return nil, "", ErrPreviewAccess
		}
		return nil, "", err
	}
	closeBody := true
	defer func() {
		if closeBody {
			resp.Body.Close()
		}
	}()
	if resp.ContentLength > maxVideoPreviewBytes {
		return nil, "", fmt.Errorf("attachment exceeds download size limit")
	}
	// Classify the response before reading its body so only videos get the larger budget.
	const sniffBytes = 512 // http.DetectContentType's sniff window
	body := bufio.NewReaderSize(resp.Body, sniffBytes)
	prefix, err := body.Peek(sniffBytes)
	if err != nil && err != io.EOF {
		return nil, "", fmt.Errorf("read attachment response failed")
	}
	if len(prefix) == 0 {
		return nil, "", fmt.Errorf("attachment is empty")
	}
	detected := http.DetectContentType(prefix)
	contentType := resp.Header.Get("Content-Type")
	declared, _, _ := mime.ParseMediaType(contentType)
	mediaType := ""
	if strings.HasPrefix(strings.ToLower(contentType), "text/html") ||
		strings.HasPrefix(detected, "text/html") && declared != "text/plain" {
		disposition, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
		if disposition != "attachment" {
			return nil, "", fmt.Errorf("GitHub returned HTML instead of an attachment; verify repository access")
		}
		// Leave mediaType empty so HTML uploads go through plain-text validation.
	} else {
		mediaType = previewMediaType(detected, declared, prefix)
	}
	if strings.HasPrefix(mediaType, "video/") {
		closeBody = false
		return &VideoPreviewBody{
			body:   io.LimitedReader{R: body, N: maxVideoPreviewBytes},
			length: resp.ContentLength,
			Closer: resp.Body,
			client: client,
			url:    asset.URL,
			token:  viewerToken,
		}, mediaType, nil
	}
	data, err := readAttachment(body, resp.ContentLength, MaxFileBytes)
	if err != nil {
		return nil, "", err
	}
	if mediaType != "" {
		return io.NopCloser(bytes.NewReader(data)), mediaType, nil
	}
	if utf8.Valid(data) && bytes.IndexFunc(data, func(r rune) bool {
		return unicode.IsControl(r) && r != '\t' && r != '\n' && r != '\r'
	}) == -1 {
		return io.NopCloser(bytes.NewReader(data)), "text/plain; charset=utf-8", nil
	}
	return nil, "", fmt.Errorf("attachment format is not supported for previews")
}

// VideoPreviewBody streams a sniffed video attachment.
type VideoPreviewBody struct {
	body   io.LimitedReader
	length int64
	io.Closer
	client *http.Client
	url    string
	token  string
}

func (b *VideoPreviewBody) ContentLength() int64 { return b.length }

// OpenRange requests an inclusive byte range from the upstream attachment.
// A nil body with no error means the upstream ignored the range.
func (b *VideoPreviewBody) OpenRange(ctx context.Context, start, end int64) (io.ReadCloser, error) {
	resp, err := fetch(ctx, b.client, b.url, b.token, fmt.Sprintf("bytes=%d-%d", start, end))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		resp.Body.Close()
		return nil, nil
	}
	wantRange := fmt.Sprintf("bytes %d-%d/%d", start, end, b.length)
	encoding := resp.Header.Get("Content-Encoding")
	if resp.Header.Get("Content-Range") != wantRange ||
		(resp.ContentLength >= 0 && resp.ContentLength != end-start+1) ||
		(resp.Uncompressed || encoding != "" && !strings.EqualFold(encoding, "identity")) {
		resp.Body.Close()
		return nil, fmt.Errorf("invalid attachment range response")
	}
	return resp.Body, nil
}

func (b *VideoPreviewBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	var n int
	var err error
	if b.body.N > 0 {
		n, err = b.body.Read(p)
	} else {
		// Distinguish a complete video at the limit from an oversized one.
		var extra [1]byte
		if _, err = io.ReadFull(b.body.R, extra[:]); err == nil {
			return 0, fmt.Errorf("attachment exceeds download size limit")
		}
	}
	if err != nil && err != io.EOF {
		return n, fmt.Errorf("read attachment response failed")
	}
	return n, err
}

func previewMediaType(detected, declared string, prefix []byte) string {
	// GitHub can serve AVIF bytes under a JPEG content type.
	if isAVIF(prefix) {
		return "image/avif"
	}
	switch detected {
	case "application/pdf", "audio/mpeg", "image/bmp", "image/gif", "image/jpeg", "image/png", "image/webp", "image/x-icon", "video/mp4", "video/webm":
		return detected
	case "audio/wave":
		return "audio/wav"
	case "application/ogg":
		return "video/ogg"
	}
	// Some browser media formats have no signature in http.DetectContentType.
	switch declared {
	case "audio/mpeg", "image/avif", "image/svg+xml", "image/vnd.microsoft.icon", "video/ogg", "video/quicktime", "video/x-m4v":
		return declared
	}
	// Go sniffs ID3 tags but misses MP3 streams that start with an MPEG Layer III frame.
	if isMP3Frame(prefix) {
		return "audio/mpeg"
	}
	return ""
}

func isAVIF(prefix []byte) bool {
	if len(prefix) < 16 || string(prefix[4:8]) != "ftyp" {
		return false
	}
	boxSize := uint64(binary.BigEndian.Uint32(prefix[:4]))
	headerSize := 8
	if boxSize == 1 {
		boxSize = binary.BigEndian.Uint64(prefix[8:16])
		headerSize = 16
	}
	brandsStart := headerSize + 8 // Major brand and minor version precede compatible brands.
	if boxSize < uint64(brandsStart) || boxSize%4 != 0 || boxSize > uint64(len(prefix)) {
		return false
	}
	isAVIFBrand := func(brand []byte) bool {
		return string(brand) == "avif" || string(brand) == "avis"
	}
	if isAVIFBrand(prefix[headerSize : headerSize+4]) {
		return true
	}
	for offset := brandsStart; offset < int(boxSize); offset += 4 {
		if isAVIFBrand(prefix[offset : offset+4]) {
			return true
		}
	}
	return false
}

func isMP3Frame(data []byte) bool {
	if len(data) < 4 || data[0] != 0xff || data[1]&0xe0 != 0xe0 {
		return false
	}
	version, layer := (data[1]>>3)&3, (data[1]>>1)&3
	bitrate, sampleRate := data[2]>>4, (data[2]>>2)&3
	return version != 1 && layer == 1 && bitrate != 15 && sampleRate != 3 && data[3]&3 != 2
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
	resp, err := fetch(ctx, client, asset.URL, token, "")
	if err != nil {
		return File{}, 0, err
	}
	defer resp.Body.Close()
	data, err := readAttachment(resp.Body, resp.ContentLength, limit)
	if err != nil {
		return File{}, 0, err
	}
	headers := resp.Header
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

type attachmentHTTPError struct {
	status       int
	githubAccess bool
}

func (e *attachmentHTTPError) Error() string {
	return fmt.Sprintf("GitHub returned HTTP %d; verify attachment availability and repository access", e.status)
}

func fetch(ctx context.Context, client *http.Client, rawURL, token, byteRange string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid attachment request")
	}
	if req.URL.Host == "github.com" && token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/octet-stream")
	if byteRange != "" {
		req.Header.Set("Range", byteRange)
		req.Header.Set("Accept-Encoding", "identity")
	}
	resp, err := client.Do(req)
	if err != nil {
		// net/http errors contain URLs, including signed redirect credentials.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrPreviewAccess) {
			return nil, ErrPreviewAccess
		}
		return nil, fmt.Errorf("attachment request failed (network, timeout, or disallowed redirect)")
	}
	if resp.StatusCode != http.StatusOK && (byteRange == "" || resp.StatusCode != http.StatusPartialContent) {
		defer resp.Body.Close()
		if resp.Request != nil {
			req = resp.Request
		}
		access := resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound
		rateLimited := resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-RateLimit-Remaining") == "0"
		return nil, &attachmentHTTPError{status: resp.StatusCode, githubAccess: req.URL.Host == "github.com" && access && !rateLimited}
	}
	return resp, nil
}

func readAttachment(body io.Reader, contentLength, limit int64) ([]byte, error) {
	if contentLength > limit {
		return nil, fmt.Errorf("attachment exceeds download size limit")
	}
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read attachment response failed")
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("attachment exceeds download size limit")
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("attachment is empty")
	}
	return data, nil
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
