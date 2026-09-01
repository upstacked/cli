package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/upstacked/cli/internal/errs"
	"gopkg.in/yaml.v3"
)

const (
	configFileName = "config.yaml"
	credsFileName  = "credentials.yaml"
	// credsPerm keeps tokens readable only by the owner. Enforced on write and
	// checked by `ups doctor`.
	credsPerm fs.FileMode = 0o600
	dirPerm   fs.FileMode = 0o700
)

// Store is a directory holding config and credentials.
type Store struct{ Dir string }

// DefaultDir honours XDG_CONFIG_HOME, falling back to ~/.config/ups.
func DefaultDir() (string, error) {
	if d := os.Getenv("UPS_CONFIG_DIR"); d != "" {
		return d, nil
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "ups"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errs.General("cannot determine home directory").Wrapping(err)
	}
	return filepath.Join(home, ".config", "ups"), nil
}

func NewStore(dir string) (*Store, error) {
	if dir == "" {
		d, err := DefaultDir()
		if err != nil {
			return nil, err
		}
		dir = d
	}
	return &Store{Dir: dir}, nil
}

func (s *Store) ConfigPath() string { return filepath.Join(s.Dir, configFileName) }
func (s *Store) CredsPath() string  { return filepath.Join(s.Dir, credsFileName) }

// LoadConfig returns an empty config when the file does not exist, so that
// first run is not an error.
func (s *Store) LoadConfig() (*File, error) {
	b, err := os.ReadFile(s.ConfigPath())
	if errors.Is(err, fs.ErrNotExist) {
		return NewFile(), nil
	}
	if err != nil {
		return nil, errs.General("cannot read %s", s.ConfigPath()).Wrapping(err)
	}
	f := NewFile()
	if err := yaml.Unmarshal(b, f); err != nil {
		return nil, errs.General("cannot parse %s", s.ConfigPath()).
			WithHint("fix the YAML, or delete the file and run: ups init").Wrapping(err)
	}
	if f.Profiles == nil {
		f.Profiles = map[string]*Profile{}
	}
	return f, nil
}

func (s *Store) SaveConfig(f *File) error {
	if err := os.MkdirAll(s.Dir, dirPerm); err != nil {
		return errs.General("cannot create %s", s.Dir).Wrapping(err)
	}
	b, err := yaml.Marshal(f)
	if err != nil {
		return errs.General("cannot serialize config").Wrapping(err)
	}
	return atomicWrite(s.ConfigPath(), b, 0o644)
}

// Credential is a token set bound to the server that issued it.
type Credential struct {
	// APIURL is the binding. A token is never sent to a different host
	// (requirement X3b) - see Credentials.For.
	APIURL   string    `yaml:"api_url"`
	Access   string    `yaml:"access"`
	Refresh  string    `yaml:"refresh,omitempty"`
	Username string    `yaml:"username,omitempty"`
	Obtained time.Time `yaml:"obtained,omitempty"`
}

type Credentials struct {
	Credentials map[string]*Credential `yaml:"credentials"`
}

func (s *Store) LoadCredentials() (*Credentials, error) {
	b, err := os.ReadFile(s.CredsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return &Credentials{Credentials: map[string]*Credential{}}, nil
	}
	if err != nil {
		return nil, errs.General("cannot read %s", s.CredsPath()).Wrapping(err)
	}
	c := &Credentials{}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, errs.General("cannot parse %s", s.CredsPath()).
			WithHint("delete the file and run: ups login").Wrapping(err)
	}
	if c.Credentials == nil {
		c.Credentials = map[string]*Credential{}
	}
	return c, nil
}

func (s *Store) SaveCredentials(c *Credentials) error {
	if err := os.MkdirAll(s.Dir, dirPerm); err != nil {
		return errs.General("cannot create %s", s.Dir).Wrapping(err)
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return errs.General("cannot serialize credentials").Wrapping(err)
	}
	return atomicWrite(s.CredsPath(), b, credsPerm)
}

// For returns the stored credential for a profile only if it was issued by the
// server currently in use. Returning nil on mismatch is the safeguard that
// stops a production token being replayed against another host.
func (c *Credentials) For(profile, apiURL string) (*Credential, bool) {
	cred, ok := c.Credentials[profile]
	if !ok || cred == nil || cred.Access == "" {
		return nil, false
	}
	if !SameHost(cred.APIURL, apiURL) {
		return nil, false
	}
	return cred, true
}

// Stored reports a credential regardless of host binding, so that diagnostics
// can tell "no token" apart from "token for a different server".
func (c *Credentials) Stored(profile string) (*Credential, bool) {
	cred, ok := c.Credentials[profile]
	if !ok || cred == nil || cred.Access == "" {
		return nil, false
	}
	return cred, true
}

func (c *Credentials) Set(profile string, cred *Credential) {
	if c.Credentials == nil {
		c.Credentials = map[string]*Credential{}
	}
	c.Credentials[profile] = cred
}

func (c *Credentials) Delete(profile string) { delete(c.Credentials, profile) }

// SameHost compares two server URLs after normalization.
func SameHost(a, b string) bool {
	na, ea := NormalizeURL(a)
	nb, eb := NormalizeURL(b)
	if ea != nil || eb != nil {
		return false
	}
	return na == nb
}

// atomicWrite avoids leaving a half-written config or credential file behind.
func atomicWrite(path string, b []byte, perm fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return errs.General("cannot write %s", path).Wrapping(err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return errs.General("cannot write %s", path).Wrapping(err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return errs.General("cannot set permissions on %s", path).Wrapping(err)
	}
	if err := tmp.Close(); err != nil {
		return errs.General("cannot write %s", path).Wrapping(err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return errs.General("cannot replace %s", path).Wrapping(err)
	}
	return nil
}

// FileMode reports the permission bits of a path, for doctor's checks.
func FileMode(path string) (fs.FileMode, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Mode().Perm(), nil
}
