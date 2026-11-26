package githubutil

import (
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

type cliResponse struct {
	family  string
	status  int
	headers http.Header
}

func (r cliResponse) bucket() string {
	if r.family != "rest" {
		return r.family
	}
	switch resource := r.headers.Get("X-RateLimit-Resource"); resource {
	case "core", "search", "code_search", "integration_manifest", "dependency_snapshots", "code_scanning_upload", "actions_runner_registration", "scim":
		return resource
	default:
		return "rest"
	}
}

// GitHub CLI writes one GH_DEBUG=api trace per HTTP exchange. Keep only
// allowlisted headers and the CLI diagnostic after the final trace.
func cliResponses(raw string) ([]cliResponse, string) {
	var responses []cliResponse
	var response cliResponse
	inHeaders := false
	end := 0
	offset := 0
	for line := range strings.SplitSeq(raw, "\n") {
		offset += len(line) + 1
		switch {
		case strings.HasPrefix(line, "* Request to "):
			response = cliResponse{family: "other"}
			if u, err := url.Parse(strings.TrimPrefix(line, "* Request to ")); err == nil && u.Host != "" {
				response.family = "rest"
				if strings.HasSuffix(u.Path, "/graphql") {
					response.family = "graphql"
				}
			}
			inHeaders = false
		case strings.HasPrefix(line, "< HTTP/") && response.family != "" && response.status == 0:
			fields := strings.Fields(line)
			if len(fields) >= 3 {
				response.status, _ = strconv.Atoi(fields[2])
				response.headers = make(http.Header)
				inHeaders = true
			}
		case strings.HasPrefix(line, "* Request took "):
			if response.status != 0 {
				responses = append(responses, response)
			}
			response = cliResponse{}
			inHeaders = false
			end = min(offset, len(raw))
		case inHeaders:
			if !strings.HasPrefix(line, "< ") {
				inHeaders = false
				continue
			}
			if name, value, ok := strings.Cut(line[2:], ":"); ok {
				switch strings.ToLower(name) {
				case "x-ratelimit-resource", "x-ratelimit-limit", "x-ratelimit-remaining", "x-ratelimit-reset", "retry-after", "link":
					response.headers.Set(name, strings.TrimSpace(value))
				}
			}
		}
	}
	if response.family != "" {
		// A canceled process can leave a partial trace. It has no safe suffix.
		return responses, ""
	}
	return responses, strings.TrimSpace(raw[end:])
}

type cachedResponse struct {
	body    string
	expires time.Time
}

// Only cache account identity and immutable Git tag objects. Ownership still
// requires a fresh matching-refs read and reviews/checks always read live.
func stableRead(cmd execx.Command) (string, execx.Command) {
	if len(cmd.Env) != 0 || len(cmd.Args) < 2 || cmd.Args[0] != "api" {
		return "", cmd
	}
	switch {
	case slices.Equal(cmd.Args, []string{"api", "--cache", "10m", "user"}):
		cmd.Args = []string{"api", "user"}
	case slices.Equal(cmd.Args, []string{"api", "user", "--jq", ".login", "--cache", "10m"}):
		cmd.Args = []string{"api", "user", "--jq", ".login"}
	case len(cmd.Args) == 2 && strings.HasPrefix(cmd.Args[1], "repos/") && strings.Contains(cmd.Args[1], "/git/tags/"):
	default:
		return "", cmd
	}
	return strings.Join(cmd.Args, " "), cmd
}

func commandResource(cmd execx.Command) string {
	if len(cmd.Args) == 0 {
		return ""
	}
	if cmd.Args[0] == "api" {
		for _, arg := range cmd.Args[1:] {
			switch {
			case arg == "graphql":
				return "graphql"
			case strings.HasPrefix(arg, "search/code"):
				return "code_search"
			case strings.HasPrefix(arg, "search/"):
				return "search"
			}
		}
		return "core"
	}
	switch cmd.Args[0] {
	case "pr", "repo", "issue", "search":
		return "graphql"
	default:
		return ""
	}
}

// Keep requests on the GitHub CLI's selected API host, even when GitHub's
// pagination uses the repository's numeric path instead of owner/name.
var nextRESTPageLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"(?:,|\s*$)`)

func nextRESTPage(link string) string {
	match := nextRESTPageLink.FindStringSubmatch(link)
	if len(match) == 0 {
		return ""
	}
	u, err := url.Parse(match[1])
	if err != nil || u.RawQuery == "" || !strings.HasPrefix(u.Path, "/") {
		return ""
	}
	path := strings.TrimPrefix(strings.TrimPrefix(u.EscapedPath(), "/api/v3/"), "/")
	if path == "" {
		return ""
	}
	return path + "?" + u.RawQuery
}
