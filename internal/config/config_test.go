package config

import "testing"

func TestPrecedenceFlagBeatsEnvBeatsProfile(t *testing.T) {
	f := &File{CurrentProfile: "default", Profiles: map[string]*Profile{
		"default": {APIURL: "https://profile.example", Infrastructure: "1"},
	}}

	cases := []struct {
		name       string
		o          Overrides
		wantURL    string
		wantSource Source
	}{
		{"profile only", Overrides{}, "https://profile.example", SourceProfile},
		{"env wins over profile", Overrides{APIURLEnv: "https://env.example"},
			"https://env.example", SourceEnv},
		{"flag wins over env", Overrides{APIURLEnv: "https://env.example", APIURLFlag: "https://flag.example"},
			"https://flag.example", SourceFlag},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Resolve(f, tc.o)
			if err != nil {
				t.Fatal(err)
			}
			if r.APIURL.Value != tc.wantURL {
				t.Errorf("got %q, want %q", r.APIURL.Value, tc.wantURL)
			}
			if r.APIURL.Source != tc.wantSource {
				t.Errorf("got source %q, want %q", r.APIURL.Source, tc.wantSource)
			}
		})
	}
}

func TestAliasExpansion(t *testing.T) {
	got, err := NormalizeURL("staging")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://api.upstacked-staging.sokl.dev" {
		t.Errorf("staging alias did not expand, got %q", got)
	}
}

func TestNormalizeURL(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{"https://a.example/", "https://a.example", false},
		{"a.example", "https://a.example", false},
		{"http://a.example/api/", "http://a.example/api", false},
		{"ftp://a.example", "", true},
		{"", "", true},
	}
	for _, tc := range cases {
		got, err := NormalizeURL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("NormalizeURL(%q) should have failed", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeURL(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The host binding is a security control, so it gets a direct test.
func TestCredentialsAreBoundToTheIssuingHost(t *testing.T) {
	c := &Credentials{Credentials: map[string]*Credential{
		"default": {APIURL: "https://prod.example", Access: "secret-token"},
	}}

	if _, ok := c.For("default", "https://prod.example"); !ok {
		t.Error("a credential should be returned for the server that issued it")
	}
	if _, ok := c.For("default", "https://prod.example/"); !ok {
		t.Error("a trailing slash should not break the binding")
	}
	if _, ok := c.For("default", "https://staging.example"); ok {
		t.Error("a credential must NOT be returned for a different server")
	}
	// Stored must still see it, so diagnostics can explain the mismatch
	// instead of reporting a generic "not logged in".
	if _, ok := c.Stored("default"); !ok {
		t.Error("Stored should report the credential regardless of binding")
	}
}
