// Package history records what the CLI did, so a support question can be
// answered from evidence rather than from memory.
//
// Nothing here is telemetry: the file never leaves the machine, and the CLI
// does not read it except when asked. It records the shape of each invocation
// - the command, the requests it made, the status codes that came back - and
// deliberately not the bodies, because responses carry customer data and a
// debug log is the last place it should be duplicated.
package history

import (
	"bufio"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FileName is the log, alongside the config it describes.
const FileName = "history.jsonl"

// maxEntries is how many invocations are kept. Enough to cover a support
// conversation, small enough that the file never needs attention.
const maxEntries = 300

// Call is one API request an invocation made.
type Call struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
	MS     int64  `json:"ms"`
}

// Record is one CLI invocation.
type Record struct {
	Time     time.Time `json:"time"`
	Version  string    `json:"version"`
	Command  string    `json:"command"`
	Args     []string  `json:"args"`
	Exit     int       `json:"exit"`
	MS       int64     `json:"ms"`
	APIURL   string    `json:"api_url,omitempty"`
	Profile  string    `json:"profile,omitempty"`
	Infra    string    `json:"infrastructure,omitempty"`
	Error    string    `json:"error,omitempty"`
	Requests []Call    `json:"requests,omitempty"`
}

// Disabled reports whether recording is turned off.
func Disabled() bool {
	v := strings.ToLower(os.Getenv("UPS_NO_HISTORY"))
	return v == "1" || v == "true" || v == "yes"
}

// Path returns the log file inside a config directory.
func Path(configDir string) string { return filepath.Join(configDir, FileName) }

// Append writes one record, trimming the file when it has grown past the cap.
//
// Failure is silent by design. A CLI that cannot write its debug log has not
// failed at the thing the user asked for, and an error here would bury the one
// that matters.
func Append(configDir string, r Record) {
	if Disabled() || configDir == "" {
		return
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return
	}
	line, err := json.Marshal(r)
	if err != nil {
		return
	}
	path := Path(configDir)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
	trim(path)
}

// trim keeps the newest maxEntries lines.
func trim(path string) {
	all, err := readLines(path)
	if err != nil || len(all) <= maxEntries {
		return
	}
	keep := all[len(all)-maxEntries:]
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(keep, "\n")+"\n"), 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// Read returns the most recent n records, newest last. n <= 0 returns all.
func Read(configDir string, n int) ([]Record, error) {
	lines, err := readLines(Path(configDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]Record, 0, len(lines))
	for _, l := range lines {
		var r Record
		if json.Unmarshal([]byte(l), &r) == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			out = append(out, line)
		}
	}
	return out, sc.Err()
}

// secretish are flag names whose values must never reach the log.
//
// Secrets are supposed to arrive on stdin, so argv should already be clean.
// This is the second line: a flag added later that takes one in argv would
// otherwise be recorded here forever.
var secretish = []string{"password", "secret", "token", "community", "key", "auth"}

// RedactArgs replaces the value of any secret-looking flag.
func RedactArgs(args []string) []string {
	out := make([]string, 0, len(args))
	redactNext := false
	for _, a := range args {
		if redactNext {
			out = append(out, "***")
			redactNext = false
			continue
		}
		if strings.HasPrefix(a, "-") {
			name, value, hasValue := strings.Cut(a, "=")
			if looksSecret(name) {
				if hasValue {
					out = append(out, name+"=***")
					continue
				}
				// A bare secret flag may take the next argument, unless it is
				// the -stdin/-file form, which names a source and not a value.
				out = append(out, a)
				redactNext = !strings.HasSuffix(name, "-stdin") && !strings.HasSuffix(name, "-file")
				continue
			}
			_ = value
		}
		out = append(out, a)
	}
	return out
}

func looksSecret(flag string) bool {
	f := strings.ToLower(strings.TrimLeft(flag, "-"))
	for _, s := range secretish {
		if strings.Contains(f, s) {
			return true
		}
	}
	return false
}

// Mode returns the log file's permissions, for `ups doctor`-style checks.
func Mode(configDir string) (fs.FileMode, error) {
	fi, err := os.Stat(Path(configDir))
	if err != nil {
		return 0, err
	}
	return fi.Mode().Perm(), nil
}
