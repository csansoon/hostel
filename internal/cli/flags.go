// Package cli has small helpers for the hostel command line.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
)

// ErrUsage marks a usage error (exit code 2).
var ErrUsage = errors.New("usage error")

// NewFlagSet returns a flag set that reports errors to w and does not exit.
func NewFlagSet(name string, w io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(w)
	return fs
}

// Parse parses flags that can come before, between, or after the positional
// arguments, and returns the positional arguments. All arguments after "--" are
// positional.
func Parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUsage, err)
		}
		rest := fs.Args()
		consumed := args[:len(args)-len(rest)]
		if len(consumed) > 0 && consumed[len(consumed)-1] == "--" {
			return append(positional, rest...), nil
		}
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// StringList is a flag that can be given more than once.
type StringList []string

func (s *StringList) String() string { return fmt.Sprint([]string(*s)) }

func (s *StringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}
