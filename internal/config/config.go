// Package config resolves CLI configuration from flags, environment, and file,
// and records where each value came from.
//
// Provenance is not a nicety: staging and production differ by one line of
// config and nothing in the prompt, so `ups context show` must be able to say
// which layer won (requirement A7).
package config

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/upstacked/cli/internal/errs"
)

// DefaultAPIURL can be injected at build time:
//
//	go build -ldflags "-X github.com/upstacked/cli/internal/config.DefaultAPIURL=https://..."
//
// It is deliberately empty by default. The OpenAPI spec declares no servers
// block, so guessing a hostname would point the CLI at a domain that may not
// exist and fail in a confusing way. Requiring it is honest.
var DefaultAPIURL = ""

// Source identifies which layer supplied a value. Highest wins.
type Source string

const (
	SourceFlag    Source = "flag"
	SourceEnv     Source = "env"
	SourceProfile Source = "profile"
	SourceDefault Source = "default"
	SourceUnset   Source = "unset"
)

// Value is a resolved setting plus its provenance.
type Value struct {
	Value  string
	Source Source
}

func (v Value) String() string { return v.Value }
func (v Value) IsSet() bool    { return v.Value != "" }

// Describe renders "value (from env)" for human output.
func (v Value) Describe() string {
	if !v.IsSet() {
		return "(not set)"
	}
	return fmt.Sprintf("%s (from %s)", v.Value, v.Source)
}

// Profile is one named server + context pairing.
type Profile struct {
	APIURL             string `yaml:"api_url"`
	Infrastructure     string `yaml:"infrastructure,omitempty"`
	InfrastructureName string `yaml:"infrastructure_name,omitempty"`
	Customer           string `yaml:"customer,omitempty"`
	CustomerName       string `yaml:"customer_name,omitempty"`
}

// File is the on-disk config document.
type File struct {
	CurrentProfile string              `yaml:"current_profile"`
	Profiles       map[string]*Profile `yaml:"profiles"`
}

func NewFile() *File {
	return &File{CurrentProfile: "default", Profiles: map[string]*Profile{}}
}

// Overrides are the per-invocation flag and environment inputs.
type Overrides struct {
	Profile      string
	APIURLFlag   string
	APIURLEnv    string
	InfraFlag    string
	InfraEnv     string
	CustomerFlag string
	CustomerEnv  string
}

// Resolved is the effective configuration for one invocation.
type Resolved struct {
	ProfileName    string
	APIURL         Value
	Infrastructure Value
	Customer       Value
	Profile        *Profile
}

// Resolve applies precedence: flag > env > profile > default (requirement X3).
func Resolve(f *File, o Overrides) (*Resolved, error) {
	if f == nil {
		f = NewFile()
	}
	name := o.Profile
	if name == "" {
		name = f.CurrentProfile
	}
	if name == "" {
		name = "default"
	}

	prof := f.Profiles[name]
	if prof == nil && o.Profile != "" && len(f.Profiles) > 0 {
		return nil, errs.NotFound("profile %q does not exist", o.Profile).
			WithHint("known profiles: %s", strings.Join(profileNames(f), ", "))
	}
	if prof == nil {
		prof = &Profile{}
	}

	r := &Resolved{ProfileName: name, Profile: prof}
	r.APIURL = pick(o.APIURLFlag, o.APIURLEnv, prof.APIURL, DefaultAPIURL)
	r.Infrastructure = pick(o.InfraFlag, o.InfraEnv, prof.Infrastructure, "")
	r.Customer = pick(o.CustomerFlag, o.CustomerEnv, prof.Customer, "")

	if r.APIURL.IsSet() {
		normalized, err := NormalizeURL(r.APIURL.Value)
		if err != nil {
			return nil, err
		}
		r.APIURL.Value = normalized
	}
	return r, nil
}

func pick(flag, env, profile, def string) Value {
	switch {
	case flag != "":
		return Value{flag, SourceFlag}
	case env != "":
		return Value{env, SourceEnv}
	case profile != "":
		return Value{profile, SourceProfile}
	case def != "":
		return Value{def, SourceDefault}
	}
	return Value{"", SourceUnset}
}

func profileNames(f *File) []string {
	out := make([]string, 0, len(f.Profiles))
	for k := range f.Profiles {
		out = append(out, k)
	}
	return out
}

// RequireAPIURL returns an actionable error when no server is configured.
func (r *Resolved) RequireAPIURL() (string, error) {
	if !r.APIURL.IsSet() {
		return "", errs.Usage("no Upstacked server configured").
			WithHint("run: ups init --api-url https://your-upstacked-host")
	}
	return r.APIURL.Value, nil
}

// RequireInfra returns the active infrastructure or explains how to set one.
func (r *Resolved) RequireInfra() (string, error) {
	if !r.Infrastructure.IsSet() {
		return "", errs.Usage("no infrastructure selected").
			WithHint("run: ups context set --infra <id>   (or pass --infra)")
	}
	return r.Infrastructure.Value, nil
}

// Aliases are short names for known Upstacked servers, so operators do not
// retype hostnames. An alias is expanded before normalization.
var Aliases = map[string]string{
	"staging": "https://api.upstacked-staging.sokl.dev",
}

// ExpandAlias resolves a known short name, or returns the input unchanged.
func ExpandAlias(raw string) string {
	if v, ok := Aliases[strings.ToLower(strings.TrimSpace(raw))]; ok {
		return v
	}
	return raw
}

// AliasNames lists known aliases for help text and error hints.
func AliasNames() []string {
	out := make([]string, 0, len(Aliases))
	for k := range Aliases {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// NormalizeURL validates a server URL and strips a trailing slash so that
// credential-to-host binding compares equal across spellings.
func NormalizeURL(raw string) (string, error) {
	raw = ExpandAlias(raw)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errs.Usage("API URL is empty")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errs.Usage("invalid API URL %q", raw).Wrapping(err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errs.Usage("API URL must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return "", errs.Usage("API URL has no host: %q", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}
