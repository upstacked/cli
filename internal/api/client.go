// Package api is the HTTP transport for the Upstacked API.
//
// It knows about authentication, pagination and error mapping, and nothing
// about commands or rendering. Keeping that boundary is what lets the command
// layer be tested against a stub server.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/upstacked/cli/internal/errs"
)

const (
	// MaxPages caps pagination traversal. An explicit limit beats an unbounded
	// loop over a paginated endpoint (Tigerstyle). Callers are told when a
	// result set was truncated rather than being handed a silent partial.
	MaxPages = 100
	// DefaultPageSize is requested where the endpoint supports limit/offset.
	DefaultPageSize = 100
	// DefaultTimeout bounds every request.
	DefaultTimeout = 30 * time.Second
)

// Version is overridden at build time via -ldflags.
var Version = "dev"

// UserAgent identifies the CLI in server logs.
func UserAgent() string { return "ups/" + Version }

// TokenSource supplies and refreshes credentials.
type TokenSource interface {
	Access() string
	Refresh(ctx context.Context, c *Client) error
}

// Client talks to one Upstacked server.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Tokens  TokenSource
	// Debug writes request lines to this writer when non-nil.
	Debug io.Writer
}

func New(baseURL string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: timeout},
	}
}

// Request is one API call.
type Request struct {
	Method string
	Path   string
	Query  url.Values
	Body   any
	// NoAuth skips the Authorization header, for login and public endpoints.
	NoAuth bool
}

// Page is the shape every paginated Upstacked list endpoint returns.
type Page struct {
	Count    int               `json:"count"`
	Next     *string           `json:"next"`
	Previous *string           `json:"previous"`
	Results  []json.RawMessage `json:"results"`
}

// List is a fully traversed result set plus whether traversal hit the cap.
type List struct {
	Items     []json.RawMessage
	Count     int
	Truncated bool
}

// Do performs a request and decodes a JSON response into out (may be nil).
func (c *Client) Do(ctx context.Context, r Request, out any) error {
	body, err := c.doRaw(ctx, r)
	if err != nil {
		return err
	}
	defer body.Close()
	raw, err := io.ReadAll(body)
	if err != nil {
		return errs.General("reading response from %s", r.Path).Wrapping(err)
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	// Unknown fields are ignored on purpose: several endpoints in the spec
	// declare no response schema, so the client must tolerate what it has not
	// been told about rather than fail to parse.
	if err := json.Unmarshal(raw, out); err != nil {
		return errs.General("unexpected response from %s", r.Path).
			WithHint("the server returned something this CLI version does not understand").
			Wrapping(err)
	}
	return nil
}

// doRaw performs the request, retrying once after a token refresh on 401.
func (c *Client) doRaw(ctx context.Context, r Request) (io.ReadCloser, error) {
	resp, err := c.send(ctx, r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized && !r.NoAuth && c.Tokens != nil {
		resp.Body.Close()
		if rerr := c.Tokens.Refresh(ctx, c); rerr != nil {
			return nil, errs.Auth("authentication failed and the session could not be renewed").
				WithHint("run: ups login").Wrapping(rerr)
		}
		if resp, err = c.send(ctx, r); err != nil {
			return nil, err
		}
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return nil, httpError(r, resp.StatusCode, b)
	}
	return resp.Body, nil
}

func (c *Client) send(ctx context.Context, r Request) (*http.Response, error) {
	if c.BaseURL == "" {
		return nil, errs.Usage("no Upstacked server configured").
			WithHint("run: ups init --api-url https://your-upstacked-host")
	}
	u := c.BaseURL + r.Path
	if len(r.Query) > 0 {
		u += "?" + r.Query.Encode()
	}

	var rdr io.Reader
	if r.Body != nil {
		b, err := json.Marshal(r.Body)
		if err != nil {
			return nil, errs.General("cannot serialize request body").Wrapping(err)
		}
		rdr = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, r.Method, u, rdr)
	if err != nil {
		return nil, errs.Usage("invalid request for %s", r.Path).Wrapping(err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent())
	if r.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if !r.NoAuth && c.Tokens != nil && c.Tokens.Access() != "" {
		req.Header.Set("Authorization", "Bearer "+c.Tokens.Access())
	}
	if c.Debug != nil {
		fmt.Fprintf(c.Debug, "> %s %s\n", r.Method, u)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errs.General("request to %s timed out or was cancelled", r.Path).
				WithHint("increase --timeout, or check connectivity to %s", c.BaseURL)
		}
		return nil, errs.General("cannot reach %s", c.BaseURL).
			WithHint("check the server URL and your network: ups context show").Wrapping(err)
	}
	if c.Debug != nil {
		fmt.Fprintf(c.Debug, "< %s\n", resp.Status)
	}
	return resp, nil
}

// GetPaged traverses a paginated endpoint up to MaxPages, reporting truncation
// rather than silently returning a partial set.
func (c *Client) GetPaged(ctx context.Context, r Request, limit int) (*List, error) {
	out := &List{}
	q := url.Values{}
	for k, v := range r.Query {
		q[k] = v
	}
	next := c.BaseURL + r.Path
	if len(q) > 0 {
		next += "?" + q.Encode()
	}

	for page := 0; ; page++ {
		if page >= MaxPages {
			out.Truncated = true
			break
		}
		var p Page
		req := Request{Method: http.MethodGet, Path: strings.TrimPrefix(next, c.BaseURL), NoAuth: r.NoAuth}
		// A `next` link may point at a different host prefix; fall back to the
		// absolute URL when it does not share our base.
		if !strings.HasPrefix(next, c.BaseURL) {
			req.Path = next
		}
		if err := c.Do(ctx, req, &p); err != nil {
			return nil, err
		}
		out.Count = p.Count
		out.Items = append(out.Items, p.Results...)

		if limit > 0 && len(out.Items) >= limit {
			out.Items = out.Items[:limit]
			out.Truncated = p.Next != nil || out.Count > limit
			break
		}
		if p.Next == nil || *p.Next == "" {
			break
		}
		next = *p.Next
	}
	return out, nil
}

// GetList handles endpoints that return either a bare array or a page object.
func (c *Client) GetList(ctx context.Context, r Request, limit int) (*List, error) {
	body, err := c.doRaw(ctx, r)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, errs.General("reading response from %s", r.Path).Wrapping(err)
	}
	trimmed := bytes.TrimSpace(raw)
	out := &List{}
	if len(trimmed) == 0 {
		return out, nil
	}
	if trimmed[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, errs.General("unexpected response from %s", r.Path).Wrapping(err)
		}
		out.Items, out.Count = arr, len(arr)
		if limit > 0 && len(out.Items) > limit {
			out.Items, out.Truncated = out.Items[:limit], true
		}
		return out, nil
	}
	var p Page
	if err := json.Unmarshal(trimmed, &p); err != nil {
		return nil, errs.General("unexpected response from %s", r.Path).Wrapping(err)
	}
	// Endpoints that return a page object are traversed properly.
	if p.Next != nil && *p.Next != "" {
		return c.GetPaged(ctx, r, limit)
	}
	out.Items, out.Count = p.Results, p.Count
	if limit > 0 && len(out.Items) > limit {
		out.Items, out.Truncated = out.Items[:limit], true
	}
	return out, nil
}

// httpError turns a status code into an actionable, exit-coded error (X5, X6).
//
// The status is carried on the error as well as the exit code, because a
// caller that supports more than one generation of the API has to distinguish
// "this server does not have that endpoint" from "that record does not exist".
func httpError(r Request, status int, body []byte) error {
	detail := extractDetail(body)
	var e *errs.Error
	switch {
	case status == http.StatusUnauthorized:
		e = errs.Auth("not authenticated (%s)", orDefault(detail, "401")).
			WithHint("run: ups login")
	case status == http.StatusForbidden:
		e = errs.Auth("permission denied for %s (%s)", r.Path, orDefault(detail, "403")).
			WithHint("check your roles: ups whoami")
	case status == http.StatusNotFound:
		e = errs.NotFound("not found: %s%s", r.Path, parenthetical(detail)).
			WithHint("verify the id and the active context: ups context show")
	case status == http.StatusConflict, status == http.StatusPreconditionFailed:
		e = errs.Conflict("conflict on %s%s", r.Path, parenthetical(detail))
	case status == http.StatusTooManyRequests:
		e = errs.General("rate limited by the server").
			WithHint("wait and retry, or reduce concurrency")
	case status >= 500:
		e = errs.General("server error %d from %s%s", status, r.Path, parenthetical(detail)).
			WithHint("this is a server-side failure; retry, then report it if it persists")
	default:
		e = errs.Usage("request rejected (%d) for %s%s", status, r.Path, parenthetical(detail))
	}
	return e.WithStatus(status)
}

// extractDetail pulls a human message out of an error body.
func extractDetail(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return ""
	}
	// Framework error pages are HTML. Dumping markup into a terminal message
	// tells the user nothing, so it is summarised instead.
	if trimmed[0] == '<' {
		if title := between(string(trimmed), "<title>", "</title>"); title != "" {
			return strings.TrimSpace(title)
		}
		return "the server returned an HTML error page"
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err == nil {
		for _, k := range []string{"detail", "message", "error", "non_field_errors"} {
			if v, ok := m[k]; ok {
				return flatten(v)
			}
		}
		// Field-level validation errors: report them all, sorted for stability.
		var parts []string
		for k, v := range m {
			parts = append(parts, k+": "+flatten(v))
		}
		if len(parts) > 0 {
			sortStrings(parts)
			return strings.Join(parts, "; ")
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

func flatten(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var parts []string
		for _, e := range t {
			parts = append(parts, flatten(e))
		}
		return strings.Join(parts, ", ")
	default:
		return fmt.Sprint(v)
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// between extracts the text between two markers, if both are present.
func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func parenthetical(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}
