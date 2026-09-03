// Package errs defines the CLI's exit-code contract.
//
// Exit codes are part of the public interface: scripts branch on them, so they
// must stay stable. See docs/user-stories.md, requirement X6.
package errs

import (
	"errors"
	"fmt"
)

const (
	CodeOK       = 0
	CodeGeneral  = 1
	CodeUsage    = 2
	CodeAuth     = 3
	CodeNotFound = 4
	CodeConflict = 5
)

// Error carries an exit code plus a remedy the user can act on.
type Error struct {
	Code   int
	Msg    string
	Hint   string
	Causer error
	// Status is the HTTP status this error came from, when it came from one.
	// The exit code alone cannot answer "does this server have that endpoint?":
	// a missing endpoint and a missing record are both CodeNotFound. A caller
	// that must degrade to an older API needs to tell those apart.
	Status int
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.Causer }

func New(code int, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// WithHint attaches the "what to try next" line required by X5.
func (e *Error) WithHint(format string, args ...any) *Error {
	e.Hint = fmt.Sprintf(format, args...)
	return e
}

func (e *Error) Wrapping(err error) *Error {
	e.Causer = err
	return e
}

// WithStatus records the HTTP status the error was built from.
func (e *Error) WithStatus(status int) *Error {
	e.Status = status
	return e
}

func Usage(format string, args ...any) *Error    { return New(CodeUsage, format, args...) }
func Auth(format string, args ...any) *Error     { return New(CodeAuth, format, args...) }
func NotFound(format string, args ...any) *Error { return New(CodeNotFound, format, args...) }
func Conflict(format string, args ...any) *Error { return New(CodeConflict, format, args...) }
func General(format string, args ...any) *Error  { return New(CodeGeneral, format, args...) }

// CodeOf extracts the exit code from any error, defaulting to CodeGeneral.
func CodeOf(err error) int {
	if err == nil {
		return CodeOK
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeGeneral
}

// StatusOf extracts the HTTP status behind an error, or 0 when it did not
// come from an HTTP response.
func StatusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// HintOf extracts the remedy line, if any.
func HintOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Hint
	}
	return ""
}
