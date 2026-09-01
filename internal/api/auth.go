package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/upstacked/cli/internal/errs"
)

// LoginResult is what the token endpoint hands back.
//
// The generated spec declares only `username` on CustomJWT, but the endpoint is
// a JWT pair endpoint. Decoding is therefore lenient: several field spellings
// are accepted so a server that names them differently still works.
type LoginResult struct {
	Access   string `json:"access"`
	Refresh  string `json:"refresh"`
	Token    string `json:"token"`
	Username string `json:"username"`
}

// AccessToken returns whichever field carried the access token.
func (l LoginResult) AccessToken() string {
	if l.Access != "" {
		return l.Access
	}
	return l.Token
}

// Login exchanges username and password for a token pair.
func Login(ctx context.Context, c *Client, username, password string) (*LoginResult, error) {
	var out LoginResult
	err := c.Do(ctx, Request{
		Method: http.MethodPost,
		Path:   "/api/token/",
		Body:   map[string]string{"username": username, "password": password},
		NoAuth: true,
	}, &out)
	if err != nil {
		var e *errs.Error
		if ok := asErr(err, &e); ok && e.Code == errs.CodeAuth {
			return nil, errs.Auth("login rejected by %s", c.BaseURL).
				WithHint("check the username and password, and that this is the right server")
		}
		return nil, err
	}
	if out.AccessToken() == "" {
		return nil, errs.Auth("the server accepted the login but returned no access token").
			WithHint("this CLI expects a JWT pair from POST /api/token/")
	}
	return &out, nil
}

// StaticTokens is a TokenSource backed by stored credentials.
type StaticTokens struct {
	AccessToken  string
	RefreshToken string
	// OnRefresh persists a renewed access token, if set.
	OnRefresh func(access string) error
}

func (s *StaticTokens) Access() string { return s.AccessToken }

// Refresh exchanges the refresh token for a new access token.
func (s *StaticTokens) Refresh(ctx context.Context, c *Client) error {
	if s.RefreshToken == "" {
		return errs.Auth("session expired and no refresh token is stored").
			WithHint("run: ups login")
	}
	var out struct {
		Access string `json:"access"`
	}
	err := c.Do(ctx, Request{
		Method: http.MethodPost,
		Path:   "/api/token/refresh/",
		Body:   map[string]string{"refresh": s.RefreshToken},
		NoAuth: true,
	}, &out)
	if err != nil {
		return err
	}
	if out.Access == "" {
		return errs.Auth("refresh returned no access token").WithHint("run: ups login")
	}
	s.AccessToken = out.Access
	if s.OnRefresh != nil {
		return s.OnRefresh(out.Access)
	}
	return nil
}

// JWTExpiry reads the `exp` claim without verifying the signature. The CLI is
// not the authority on token validity - this is only used to warn the user
// before a token lapses mid-task.
func JWTExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

func asErr(err error, target **errs.Error) bool {
	for err != nil {
		if e, ok := err.(*errs.Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
