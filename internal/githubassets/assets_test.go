package githubassets

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDiscover(t *testing.T) {
	t.Parallel()
	image := "https://github.com/user-attachments/assets/image-id"
	pdf := "https://github.com/user-attachments/files/42/spec.pdf"
	legacy := "https://user-images.githubusercontent.com/42/shot.png"
	old := "https://github.com/acme/repo/assets/42/shot-id"
	got := Discover([]Source{
		{"Issue description", "![shot](" + image + ")\n[spec](" + pdf + ")\n<img src=\"" + legacy + "\">\nhttps://example.com/a.png\nhttps://github.com/acme/repo/issues/1"},
		{"Comment 1", image + "\n<" + old + ">\n[copy](" + pdf + "#page=2)"},
	})
	want := []Asset{{image, "image-id", "Issue description"}, {pdf, "spec.pdf", "Issue description"}, {legacy, "shot.png", "Issue description"}, {old, "shot-id", "Comment 1"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("assets = %#v, want %#v", got, want)
	}
	if err := Validate(got); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsUnsafeURLs(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"http://github.com/user-attachments/assets/id", "https://github.com.evil.test/user-attachments/assets/id",
		"https://github.com:444/user-attachments/assets/id", "https://user:password@github.com/user-attachments/assets/id",
		"https://127.0.0.1/test.png", "file:///tmp/secret", "https://github.com/login",
		"https://github.com/user-attachments/files/42/%2e%2e/%2e%2e/login", "https://github.com/user-attachments/assets/..",
	} {
		if err := Validate([]Asset{{URL: raw}}); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	if err := Validate(make([]Asset, MaxAssets+1)); err == nil {
		t.Fatal("accepted too many assets")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func response(req *http.Request, status int, body string, headers http.Header) *http.Response {
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: req, ContentLength: -1}
}

func TestDownloadFilesAndRedirectAuthentication(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	assets := []Asset{{URL: "https://github.com/user-attachments/assets/id", Name: "id", Source: "Issue description"}, {URL: "https://github.com/user-attachments/files/42/spec.pdf", Name: "spec.pdf", Source: "Comment 1"}}
	calls := 0
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Host == "github.com" && req.Header.Get("Authorization") != "Bearer test-secret" {
			t.Fatal("missing GitHub authentication")
		}
		if req.URL.Host == "objects.githubusercontent.com" {
			if req.Header.Get("Authorization") != "" {
				t.Fatal("credential leaked to storage")
			}
			return response(req, 200, "\x89PNG\r\n\x1a\nimage", http.Header{"Content-Disposition": {`attachment; filename="../../shot.png"`}}), nil
		}
		if strings.Contains(req.URL.Path, "/assets/") {
			return response(req, 302, "", http.Header{"Location": {"https://objects.githubusercontent.com/signed?secret=value"}}), nil
		}
		return response(req, 200, "%PDF-1.4\nexample", nil), nil
	})
	files, err := Download(context.Background(), assets, dir, "test-secret", transport)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || len(files) != 2 || !files[0].IsImage() || files[1].IsImage() {
		t.Fatalf("unexpected downloads: %#v calls=%d", files, calls)
	}
	if files[0].Path != filepath.Join(dir, "01-shot.png") || files[1].MediaType != "application/pdf" {
		t.Fatalf("unexpected files: %#v", files)
	}
	for _, file := range files {
		data, err := os.ReadFile(file.Path)
		if err != nil || len(data) == 0 {
			t.Fatalf("missing download: %v", err)
		}
	}
}

func TestDownloadFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		headers http.Header
		want    string
	}{
		{"missing", 404, "private information", nil, "HTTP 404"},
		{"login", 200, "<!DOCTYPE html><html>login</html>", nil, "HTML"},
		{"empty", 200, "", nil, "empty"},
		{"unsafe redirect", 302, "", http.Header{"Location": {"http://127.0.0.1/secret?token=secret-value"}}, "disallowed redirect"},
		{"login redirect", 302, "", http.Header{"Location": {"https://github.com/login"}}, "disallowed redirect"},
		{"large body", 200, strings.Repeat("x", int(MaxFileBytes+1)), nil, "size limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				return response(req, tc.status, tc.body, tc.headers), nil
			})
			_, err := Download(context.Background(), []Asset{{URL: "https://github.com/user-attachments/assets/id", Name: "id"}}, t.TempDir(), "test-secret", transport)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %s", err, tc.want)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private information") {
				t.Fatal("response or signed URL leaked")
			}
			if calls != 1 {
				t.Fatalf("unexpected redirect calls=%d", calls)
			}
		})
	}
}

func TestDownloadHonorsCancellationAndRemainingLimit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) { return nil, req.Context().Err() })
	_, err := Download(ctx, []Asset{{URL: "https://github.com/user-attachments/assets/id"}}, t.TempDir(), "", transport)
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("error=%v", err)
	}
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) { return response(req, 200, "12345", nil), nil })}
	_, _, err = downloadOne(context.Background(), client, Asset{URL: "https://github.com/user-attachments/assets/id"}, t.TempDir(), "", 0, 4)
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("remaining budget error=%v", err)
	}
	// A signed redirect URL in a transport error must never reach run logs.
	client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("https://storage.test?token=sensitive")
	})
	_, _, err = downloadOne(context.Background(), client, Asset{URL: "https://github.com/user-attachments/assets/id"}, t.TempDir(), "", 0, 4)
	if err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("unsafe error=%v", err)
	}
}

func TestPreviewText(t *testing.T) {
	t.Parallel()
	const textMediaType = "text/plain; charset=utf-8"
	for _, tc := range []struct {
		name        string
		contentType string
		body        string
		want        string
	}{
		{"UTF-8 log with generic type", "application/octet-stream", "INFO\tCafé\r\n", textMediaType},
		{"explicit plain text with markup", "text/plain", "<script>alert('example')</script>", textMediaType},
		{"HTML login page", "text/html; charset=utf-8", "Sign in to GitHub", ""},
		{"sniffed HTML login page", "application/octet-stream", "<!DOCTYPE html><html>Sign in</html>", ""},
		{"invalid UTF-8", "text/plain", "prefix\xff", ""},
		{"binary after sniff window", "text/plain", strings.Repeat("a", 512) + "\x00", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return response(req, 200, tc.body, http.Header{"Content-Type": {tc.contentType}}), nil
			})
			asset := Asset{URL: "https://github.com/user-attachments/files/42/example.txt"}
			data, mediaType, err := Preview(context.Background(), asset, transport)
			if tc.want == "" {
				if err == nil || len(data) != 0 {
					t.Fatal("unsupported content was returned")
				}
			} else if err != nil || mediaType != tc.want || string(data) != tc.body {
				t.Fatalf("preview = (%q, %q, %v), want original text as %s", data, mediaType, err, tc.want)
			}
		})
	}
}
