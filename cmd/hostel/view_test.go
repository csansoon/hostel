package main

// Unit tests (AGENTS.md allows them here): the column count of a line decides
// the cursor moves of the view, and key input can arrive split over reads; an
// E2E test sees only the bytes, not how a terminal wraps them or splits them,
// and the frame must fit the terminal height with the keys and the prompt.

import (
	"slices"
	"testing"
)

func TestParseKeys(t *testing.T) {
	for _, c := range []struct {
		in, rest string
		keys     []string
	}{
		{"\x1b[A", "", []string{keyUp}},
		{"\x1b", "\x1b", nil},
		{"\x1b[", "\x1b[", nil},
		{"\x1b\x1b[B", "", []string{keyEsc, keyDown}},
		{"\x1b[1;2Bq", "", []string{keyShiftDown, keyQuit}},
		{"e\r", "", []string{"e", keyEnter}},
	} {
		keys, rest := parseKeys(c.in)
		if !slices.Equal(keys, c.keys) || rest != c.rest {
			t.Errorf("parseKeys(%q) = %q, %q; want %q, %q", c.in, keys, rest, c.keys, c.rest)
		}
	}
}

func TestCutColumns(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 5, "abc"},
		{"abcde", 5, "abcde"},
		{"abcdef", 5, "abcd…"},
		{"ab日本", 6, "ab日本"},
		{"ab日本語", 6, "ab日…"},
		{"a🙂b🙂c", 5, "a🙂b…"},
		{"étude", 5, "étude"},
	} {
		if got := cutColumns(c.in, c.n); got != c.want {
			t.Errorf("cutColumns(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestFitLines(t *testing.T) {
	body := []string{"h", "a", "b", "❯ c", "d", "e", "f", ""}
	tail := []string{"keys", "prompt"}
	for _, c := range []struct {
		height int
		want   []string
	}{
		{10, append(slices.Clone(body), tail...)},
		{6, []string{"…", "b", "❯ c", "…", "keys", "prompt"}},
		{5, []string{"…", "❯ c", "…", "keys", "prompt"}},
		{3, []string{"❯ c", "keys", "prompt"}},
		{1, []string{"prompt"}},
	} {
		if got := fitLines(body, tail, c.height); !slices.Equal(got, c.want) {
			t.Errorf("fitLines(height %d) = %q, want %q", c.height, got, c.want)
		}
	}
}

func TestFrameRows(t *testing.T) {
	cols := []int{lineColumns("abcdefgh"), lineColumns(""), lineColumns("日本語")}
	if got := frameRows(cols, 80); got != 3 {
		t.Errorf("frameRows at 80 columns = %d, want 3", got)
	}
	// Narrower: "abcdefgh" (8) takes 2 rows, "日本語" (6) takes 2 rows.
	if got := frameRows(cols, 5); got != 5 {
		t.Errorf("frameRows at 5 columns = %d, want 5", got)
	}
}
