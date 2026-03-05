// internal/cli/output/style.go

// Package output provides formatting utilities for CLI output including
// colored terminal output and JSON formatting.
package output

import (
	"fmt"
	"io"
	"os"
)

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[0;31m"
	colorGreen  = "\033[0;32m"
	colorYellow = "\033[1;33m"
	colorCyan   = "\033[0;36m"
)

// Styler formats messages with optional color codes for terminal output.
type Styler struct {
	noColor bool
}

// NewStyler creates a new Styler. If noColor is true, ANSI color codes are omitted.
func NewStyler(noColor bool) *Styler {
	return &Styler{noColor: noColor}
}

// Success formats a success message with a green checkmark.
func (s *Styler) Success(msg string) string {
	return s.format(colorGreen, "✓", msg)
}

// Error formats an error message with a red X.
func (s *Styler) Error(msg string) string {
	return s.format(colorRed, "✗", msg)
}

// Info formats an informational message with a cyan info symbol.
func (s *Styler) Info(msg string) string {
	return s.format(colorCyan, "ℹ", msg)
}

// Warn formats a warning message with a yellow warning symbol.
func (s *Styler) Warn(msg string) string {
	return s.format(colorYellow, "⚠", msg)
}

func (s *Styler) format(color, symbol, msg string) string {
	if s.noColor {
		return fmt.Sprintf("%s %s", symbol, msg)
	}
	return fmt.Sprintf("%s%s%s %s", color, symbol, colorReset, msg)
}

func (s *Styler) Fprint(w io.Writer, msg string) {
	fmt.Fprintln(w, msg)
}

func (s *Styler) FprintSuccess(w io.Writer, msg string) {
	s.Fprint(w, s.Success(msg))
}

func (s *Styler) FprintError(w io.Writer, msg string) {
	s.Fprint(w, s.Error(msg))
}

func (s *Styler) FprintInfo(w io.Writer, msg string) {
	s.Fprint(w, s.Info(msg))
}

func (s *Styler) FprintWarn(w io.Writer, msg string) {
	s.Fprint(w, s.Warn(msg))
}

// Print to stdout
func (s *Styler) PrintSuccess(msg string) {
	s.FprintSuccess(os.Stdout, msg)
}

func (s *Styler) PrintError(msg string) {
	s.FprintError(os.Stderr, msg)
}

func (s *Styler) PrintInfo(msg string) {
	s.FprintInfo(os.Stdout, msg)
}

func (s *Styler) PrintWarn(msg string) {
	s.FprintWarn(os.Stdout, msg)
}
