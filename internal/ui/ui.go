// Package ui provides terminal presentation: colour, symbols, spinners and
// interactive selection.
//
// Every affordance here degrades: colour disappears when the output is not a
// terminal or NO_COLOR is set, spinners become silence, and selection falls
// back to an error telling the user which flag to pass. Interactivity is a
// convenience layered on top of a CLI that works fully without it.
package ui

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// Style is an ANSI style that renders as a no-op when colour is disabled.
type Style struct {
	codes   string
	enabled bool
}

func (s Style) Apply(text string) string {
	if !s.enabled || s.codes == "" {
		return text
	}
	return "\x1b[" + s.codes + "m" + text + "\x1b[0m"
}

// Theme carries the styles used across the CLI.
type Theme struct {
	Enabled bool
	Bold    Style
	Dim     Style
	Red     Style
	Green   Style
	Yellow  Style
	Blue    Style
	Cyan    Style
}

// NewTheme enables colour only for interactive terminals that permit it.
func NewTheme(w io.Writer) *Theme {
	on := ColorEnabled(w)
	mk := func(c string) Style { return Style{codes: c, enabled: on} }
	return &Theme{
		Enabled: on,
		Bold:    mk("1"),
		Dim:     mk("2"),
		Red:     mk("31"),
		Green:   mk("32"),
		Yellow:  mk("33"),
		Blue:    mk("34"),
		Cyan:    mk("36"),
	}
}

// ColorEnabled honours NO_COLOR, FORCE_COLOR, TERM=dumb and TTY detection.
func ColorEnabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("FORCE_COLOR") != "" {
		return true
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return IsTerminal(w)
}

func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// Symbols are status glyphs. ASCII fallbacks keep output readable in plain
// terminals.
type Symbols struct{ OK, Fail, Warn, Arrow, Bullet, Pointer string }

func NewSymbols(unicode bool) Symbols {
	if !unicode {
		return Symbols{OK: "[ok]", Fail: "[fail]", Warn: "[warn]", Arrow: "->", Bullet: "*", Pointer: ">"}
	}
	return Symbols{OK: "✓", Fail: "✗", Warn: "!", Arrow: "→", Bullet: "•", Pointer: "❯"}
}

// UnicodeOK guesses whether the terminal renders non-ASCII glyphs.
func UnicodeOK() bool {
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if strings.Contains(strings.ToUpper(os.Getenv(k)), "UTF") {
			return true
		}
	}
	return false
}

// Spinner shows progress for operations that take time. It writes to stderr so
// it never contaminates piped stdout, and does nothing at all when stderr is
// not a terminal.
type Spinner struct {
	w       io.Writer
	active  bool
	stop    chan struct{}
	done    chan struct{}
	mu      sync.Mutex
	message string
}

func NewSpinner(w io.Writer, message string) *Spinner {
	return &Spinner{w: w, message: message, active: IsTerminal(w)}
}

func (s *Spinner) Start() {
	if !s.active {
		return
	}
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		if !UnicodeOK() {
			frames = []string{"-", "\\", "|", "/"}
		}
		for i := 0; ; i++ {
			select {
			case <-s.stop:
				return
			case <-time.After(90 * time.Millisecond):
				s.mu.Lock()
				fmt.Fprintf(s.w, "\r\x1b[2K%s %s", frames[i%len(frames)], s.message)
				s.mu.Unlock()
			}
		}
	}()
}

// Update changes the label of a running spinner.
func (s *Spinner) Update(message string) {
	s.mu.Lock()
	s.message = message
	s.mu.Unlock()
}

func (s *Spinner) Stop() {
	if !s.active || s.stop == nil {
		return
	}
	close(s.stop)
	<-s.done
	s.mu.Lock()
	fmt.Fprint(s.w, "\r\x1b[2K")
	s.mu.Unlock()
	s.stop = nil
}

// ReadPassword reads a secret without echoing it.
func ReadPassword(fd int) (string, error) {
	b, err := term.ReadPassword(fd)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Choice is one option in a selection prompt.
type Choice struct {
	Label string
	Desc  string
	Value string
}

// Select presents an arrow-key picker on a terminal. It returns the chosen
// value, or an error when selection is impossible.
func Select(in *os.File, out io.Writer, title string, choices []Choice) (string, error) {
	if len(choices) == 0 {
		return "", fmt.Errorf("nothing to choose from")
	}
	if len(choices) == 1 {
		return choices[0].Value, nil
	}
	if !term.IsTerminal(int(in.Fd())) || !IsTerminal(out) {
		return "", fmt.Errorf("cannot prompt: not a terminal")
	}

	state, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return selectNumbered(in, out, title, choices)
	}
	defer term.Restore(int(in.Fd()), state)

	theme := NewTheme(out)
	sym := NewSymbols(UnicodeOK())
	cursor := 0
	buf := make([]byte, 3)
	first := true

	render := func() {
		if !first {
			fmt.Fprintf(out, "\x1b[%dA", len(choices))
		}
		first = false
		for i, c := range choices {
			line := c.Label
			if c.Desc != "" {
				line += "  " + theme.Dim.Apply(c.Desc)
			}
			if i == cursor {
				fmt.Fprintf(out, "\r\x1b[2K%s %s\r\n", theme.Cyan.Apply(sym.Pointer), theme.Bold.Apply(line))
			} else {
				fmt.Fprintf(out, "\r\x1b[2K  %s\r\n", line)
			}
		}
	}

	fmt.Fprintf(out, "\r\x1b[2K%s %s\r\n", theme.Bold.Apply(title), theme.Dim.Apply("(up/down, enter)"))
	render()

	for {
		n, err := in.Read(buf)
		if err != nil {
			return "", err
		}
		switch {
		case n == 1 && (buf[0] == '\r' || buf[0] == '\n'):
			return choices[cursor].Value, nil
		case n == 1 && (buf[0] == 3 || buf[0] == 27):
			return "", fmt.Errorf("cancelled")
		case n == 1 && buf[0] == 'j':
			cursor = (cursor + 1) % len(choices)
			render()
		case n == 1 && buf[0] == 'k':
			cursor = (cursor - 1 + len(choices)) % len(choices)
			render()
		case n == 3 && buf[0] == 27 && buf[1] == '[':
			switch buf[2] {
			case 'B':
				cursor = (cursor + 1) % len(choices)
				render()
			case 'A':
				cursor = (cursor - 1 + len(choices)) % len(choices)
				render()
			}
		}
	}
}

// selectNumbered is the fallback when raw mode is unavailable.
func selectNumbered(in *os.File, out io.Writer, title string, choices []Choice) (string, error) {
	fmt.Fprintf(out, "%s\n", title)
	for i, c := range choices {
		fmt.Fprintf(out, "  %d) %s %s\n", i+1, c.Label, c.Desc)
	}
	fmt.Fprint(out, "Choice: ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil {
		return "", err
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(choices) {
		return "", fmt.Errorf("invalid choice")
	}
	return choices[n-1].Value, nil
}

// Heading writes a section header.
func Heading(w io.Writer, t *Theme, text string) {
	fmt.Fprintf(w, "\n%s\n", t.Bold.Apply(text))
}

// IsTerminalFd reports whether a file descriptor is an interactive terminal.
// This is stricter than checking for a character device: /dev/null is a
// character device but is emphatically not interactive.
func IsTerminalFd(fd int) bool { return term.IsTerminal(fd) }

// MultiChoice is one toggleable option in a multi-select prompt.
type MultiChoice struct {
	Label    string
	Desc     string
	Value    string
	Selected bool
	// Disabled options are shown greyed out and cannot be toggled, so the user
	// can see that a client exists but is unavailable here, and why.
	Disabled bool
	Reason   string
}

// MultiSelect presents a checkbox list. It returns the values of the selected
// options.
//
// Callers must handle the not-a-terminal error by falling back to an explicit
// flag: an agent or CI run has no way to answer a prompt.
func MultiSelect(in *os.File, out io.Writer, title string, choices []MultiChoice) ([]string, error) {
	if len(choices) == 0 {
		return nil, fmt.Errorf("nothing to choose from")
	}
	if !term.IsTerminal(int(in.Fd())) || !IsTerminal(out) {
		return nil, fmt.Errorf("cannot prompt: not a terminal")
	}

	state, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return nil, err
	}
	defer term.Restore(int(in.Fd()), state)

	theme := NewTheme(out)
	sym := NewSymbols(UnicodeOK())
	unicode := UnicodeOK()
	checked, unchecked := "[x]", "[ ]"
	if unicode {
		checked, unchecked = "◉", "◯"
	}

	cursor := 0
	for choices[cursor].Disabled && cursor < len(choices)-1 {
		cursor++
	}

	lines := len(choices) + 1 // options plus the help line
	first := true

	render := func() {
		if !first {
			fmt.Fprintf(out, "\x1b[%dA", lines)
		}
		first = false
		for i, c := range choices {
			box := unchecked
			if c.Selected {
				box = theme.Green.Apply(checked)
			}
			label := c.Label
			if c.Desc != "" {
				label += "  " + theme.Dim.Apply(c.Desc)
			}
			if c.Disabled {
				box = theme.Dim.Apply("  -")
				label = theme.Dim.Apply(c.Label + "  " + c.Reason)
			}
			pointer := "  "
			if i == cursor {
				pointer = theme.Cyan.Apply(sym.Pointer) + " "
				if !c.Disabled {
					label = theme.Bold.Apply(label)
				}
			}
			fmt.Fprintf(out, "\r\x1b[2K%s%s %s\r\n", pointer, box, label)
		}
		fmt.Fprintf(out, "\r\x1b[2K%s\r\n",
			theme.Dim.Apply("space toggles, a all, n none, enter confirms, esc cancels"))
	}

	fmt.Fprintf(out, "\r\x1b[2K%s\r\n", theme.Bold.Apply(title))
	render()

	move := func(delta int) {
		for i := 0; i < len(choices); i++ {
			cursor = (cursor + delta + len(choices)) % len(choices)
			if !choices[cursor].Disabled {
				return
			}
		}
	}

	buf := make([]byte, 3)
	for {
		n, err := in.Read(buf)
		if err != nil {
			return nil, err
		}
		switch {
		case n == 1 && (buf[0] == '\r' || buf[0] == '\n'):
			var out []string
			for _, c := range choices {
				if c.Selected && !c.Disabled {
					out = append(out, c.Value)
				}
			}
			return out, nil
		case n == 1 && (buf[0] == 3 || buf[0] == 27):
			return nil, fmt.Errorf("cancelled")
		case n == 1 && buf[0] == ' ':
			if !choices[cursor].Disabled {
				choices[cursor].Selected = !choices[cursor].Selected
			}
			render()
		case n == 1 && (buf[0] == 'a' || buf[0] == 'A'):
			for i := range choices {
				if !choices[i].Disabled {
					choices[i].Selected = true
				}
			}
			render()
		case n == 1 && (buf[0] == 'n' || buf[0] == 'N'):
			for i := range choices {
				choices[i].Selected = false
			}
			render()
		case n == 1 && buf[0] == 'j':
			move(1)
			render()
		case n == 1 && buf[0] == 'k':
			move(-1)
			render()
		case n == 3 && buf[0] == 27 && buf[1] == '[':
			switch buf[2] {
			case 'B':
				move(1)
				render()
			case 'A':
				move(-1)
				render()
			}
		}
	}
}
