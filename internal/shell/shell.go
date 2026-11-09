// Package shell builds command lines for the shell that runs an SSH command on
// a machine: sh on macOS and Linux, and the SSH server's default shell on Windows
// (PowerShell or cmd), or the helper's PowerShell for guest commands.
package shell

import (
	"fmt"
	"strings"
)

// Kinds. The empty kind is sh.
const (
	SH         = ""
	PowerShell = "powershell"
	Cmd        = "cmd"
)

// Command returns the command line that runs path with args, with the exit
// code of the program. PowerShell itself exits with 0 or 1, so the line ends
// with an exit; a program that cannot start exits with 127, as in sh.
func Command(kind, path string, args ...string) (string, error) {
	var b strings.Builder
	switch kind {
	case SH:
		b.WriteString(quoteSH(path))
		for _, a := range args {
			b.WriteString(" " + quoteSH(a))
		}
	case PowerShell:
		b.WriteString("& " + quotePS(path))
		for _, a := range args {
			b.WriteString(" " + quotePS(a))
		}
		b.WriteString("; if ($?) { exit $LASTEXITCODE }; if ($LASTEXITCODE) { exit $LASTEXITCODE }; exit 127")
	case Cmd:
		for i, s := range append([]string{path}, args...) {
			// cmd expands %VAR% and ! also inside quotes, and has no escape
			// for a quote inside quotes.
			if strings.ContainsAny(s, "\"%!^\r\n") {
				return "", fmt.Errorf("cmd cannot pass the argument %q", s)
			}
			if i > 0 {
				b.WriteString(" ")
			}
			b.WriteString(`"` + s + `"`)
		}
	default:
		return "", fmt.Errorf("unknown shell %q", kind)
	}
	return b.String(), nil
}

// NotFound reports whether an exit code means that the program does not exist.
// cmd exits with 9009 for a missing program and with 3 for a missing directory
// (the helper's own exit codes are 0, 1, and 2).
func NotFound(kind string, code int) bool {
	if kind == Cmd {
		return code == 9009 || code == 3
	}
	return code == 126 || code == 127
}

func quoteSH(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// quotePS quotes for PowerShell: in single quotes, only ' is special (and its
// typographic forms, which PowerShell also accepts as quotes).
func quotePS(s string) string {
	r := strings.NewReplacer("'", "''", "‘", "‘‘", "’", "’’", "‚", "‚‚", "‛", "‛‛")
	return "'" + r.Replace(s) + "'"
}
