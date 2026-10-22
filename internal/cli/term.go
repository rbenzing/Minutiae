package cli

import (
	"os"
	"strings"
)

// TerminalMode says whether stderr counts as a terminal for the verify status display.
type TerminalMode int

const (
	// TerminalAuto (the zero value) detects: stderr is a console.
	TerminalAuto TerminalMode = iota
	// TerminalOn forces the status display on (tests).
	TerminalOn
	// TerminalOff forces it off (tests).
	TerminalOff
)

// errIsTerminal reports whether the status line and the outcome line may be drawn on d.Err: only when it
// is a real console (or forced on), never a pipe, a file or a buffer.
func errIsTerminal(d Deps) bool {
	switch d.ErrTerminal {
	case TerminalOn:
		return true
	case TerminalOff:
		return false
	}
	f, ok := d.Err.(*os.File)
	if !ok || f == nil {
		return false
	}
	return isTerminalFile(f)
}

// consoleUTF8 reports whether the status line may use Unicode glyphs: a UTF-8 console on Windows, a UTF-8
// locale elsewhere. Otherwise the line is ASCII.
func consoleUTF8() bool {
	if v, ok := platformConsoleUTF8(); ok {
		return v
	}
	return localeUTF8(os.Getenv)
}

// localeUTF8 reads LC_ALL, LC_CTYPE and LANG in precedence order: the first one that is set decides.
func localeUTF8(getenv func(string) string) bool {
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := getenv(k); v != "" {
			v = strings.ToLower(v)
			return strings.Contains(v, "utf-8") || strings.Contains(v, "utf8")
		}
	}
	return false
}

// defaultStatusWidth is the width assumed when the console cannot say.
const defaultStatusWidth = 79

// statusWidth is the column count of the console behind d.Err, or defaultStatusWidth when unknown.
func statusWidth(d Deps) int {
	if f, ok := d.Err.(*os.File); ok && f != nil {
		if w, ok := termWidth(f); ok {
			return w
		}
	}
	return defaultStatusWidth
}
