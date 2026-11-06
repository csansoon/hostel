package config

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

// The targeted "hostel config" commands change the document as text, so the
// comments and the order of the providers and machines (the scheduler's
// order) stay.

type tomlExpr struct {
	kind       unstable.Kind
	path       []string // the table, or the table and the keys of a key-value
	start, end int      // byte range: the header line, or "key = value"
	keyStart   int
	keyEnd     int
}

func parseExprs(data []byte) ([]tomlExpr, error) {
	var out []tomlExpr
	p := unstable.Parser{}
	p.Reset(data)
	var table []string
	for p.NextExpression() {
		e := p.Expression()
		var keys []string
		ks, ke := -1, 0
		it := e.Key()
		for it.Next() {
			n := it.Node()
			keys = append(keys, string(n.Data))
			if ks < 0 {
				ks = int(n.Raw.Offset)
			}
			ke = int(n.Raw.Offset + n.Raw.Length)
		}
		switch e.Kind {
		case unstable.Table, unstable.ArrayTable:
			table = keys
			out = append(out, tomlExpr{kind: e.Kind, path: keys, start: lineStart(data, ks), end: lineEnd(data, ke), keyStart: ks, keyEnd: ke})
		case unstable.KeyValue:
			path := append(append([]string(nil), table...), keys...)
			out = append(out, tomlExpr{kind: e.Kind, path: path, start: int(e.Raw.Offset), end: int(e.Raw.Offset + e.Raw.Length), keyStart: ks, keyEnd: ke})
		}
	}
	return out, p.Error()
}

func lineStart(data []byte, off int) int {
	return bytes.LastIndexByte(data[:off], '\n') + 1
}

func lineEnd(data []byte, off int) int {
	if i := bytes.IndexByte(data[off:], '\n'); i >= 0 {
		return off + i + 1
	}
	return len(data)
}

func hasPrefix(path, prefix []string) bool {
	return len(path) >= len(prefix) && slices.Equal(path[:len(prefix)], prefix)
}

// SetKey sets the key at path to value (TOML text, for example `"20m"` or
// `["a", "b"]`): it replaces the key's value, adds the key to its table, or
// adds the table at the end.
func SetKey(data []byte, path []string, value string) ([]byte, error) {
	data = withNewline(data)
	exprs, err := parseExprs(data)
	if err != nil {
		return nil, err
	}
	parent := path[:len(path)-1]
	insertAt, firstTable := -1, -1
	for _, e := range exprs {
		switch e.kind {
		case unstable.Table, unstable.ArrayTable:
			if firstTable < 0 {
				firstTable = commentStart(data, e.start)
			}
			if slices.Equal(e.path, parent) {
				insertAt = e.end
			}
		case unstable.KeyValue:
			if slices.Equal(e.path, path) {
				return splice(data, e.start, e.end, string(data[e.keyStart:e.keyEnd])+" = "+value), nil
			}
			if hasPrefix(path, e.path) {
				return nil, fmt.Errorf("%s is in the inline table %s; change it with \"hostel config edit\"", strings.Join(path, "."), strings.Join(e.path, "."))
			}
			if len(parent) > 0 && insertAt >= 0 && slices.Equal(e.path[:len(e.path)-1], parent) {
				insertAt = lineEnd(data, e.end)
			}
		}
	}
	key := quoteKey(path[len(path)-1])
	switch {
	case len(parent) == 0 && firstTable >= 0:
		return splice(data, firstTable, firstTable, key+" = "+value+"\n"), nil
	case len(parent) == 0:
		return append(withNewline(data), []byte(key+" = "+value+"\n")...), nil
	case insertAt >= 0:
		return splice(data, insertAt, insertAt, key+" = "+value+"\n"), nil
	}
	quoted := make([]string, len(parent))
	for i, k := range parent {
		quoted[i] = quoteKey(k)
	}
	out := withNewline(data)
	if len(out) > 0 {
		out = append(out, '\n')
	}
	return append(out, []byte("["+strings.Join(quoted, ".")+"]\n"+key+" = "+value+"\n")...), nil
}

// RemoveTable removes the table at path and its subtables, with the comment
// lines directly above each header. It reports whether there was one.
func RemoveTable(data []byte, path []string) ([]byte, bool, error) {
	exprs, err := parseExprs(data)
	if err != nil {
		return nil, false, err
	}
	type span struct{ start, end int }
	var cut []span
	for i, e := range exprs {
		if e.kind != unstable.Table && e.kind != unstable.ArrayTable || !hasPrefix(e.path, path) {
			continue
		}
		end := len(data)
		for _, next := range exprs[i+1:] {
			if next.kind == unstable.Table || next.kind == unstable.ArrayTable {
				end = commentStart(data, next.start)
				break
			}
		}
		cut = append(cut, span{commentStart(data, e.start), end})
	}
	if len(cut) == 0 {
		return data, false, nil
	}
	var out []byte
	pos := 0
	for _, c := range cut {
		if c.start < pos {
			c.start = pos
		}
		out = append(out, data[pos:c.start]...)
		pos = c.end
	}
	return append(out, data[pos:]...), true, nil
}

// commentStart moves a line start up over the comment lines directly above.
func commentStart(data []byte, start int) int {
	for start > 0 {
		prev := lineStart(data, start-1)
		if !strings.HasPrefix(strings.TrimSpace(string(data[prev:start])), "#") {
			break
		}
		start = prev
	}
	return start
}

func splice(data []byte, start, end int, text string) []byte {
	out := append([]byte(nil), data[:start]...)
	out = append(out, text...)
	return append(out, data[end:]...)
}

func withNewline(data []byte) []byte {
	out := append([]byte(nil), data...)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return out
}

func quoteKey(k string) string {
	for _, r := range k {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return fmt.Sprintf("%q", k)
		}
	}
	return k
}

// DocProvider is a provider of a configuration document, with its machines in
// document order (for the lists of "hostel config rooms" and "providers").
type DocProvider struct {
	Name     string
	Kind     string
	Machines []DocMachine
}

// DocMachine is a machine of a configuration document, as written (without
// the values that it inherits).
type DocMachine struct {
	Provider, Name, OS, Address string
	Connect                     bool
	Tools                       []string
	Spec                        map[string]any
}

// Key is "<provider>/<machine>".
func (m DocMachine) Key() string { return m.Provider + "/" + m.Name }

// DocumentProviders reads the providers and machines of a document in order.
func DocumentProviders(data []byte) ([]DocProvider, error) {
	var raw struct {
		Providers map[string]struct {
			Kind     string `toml:"kind"`
			Machines map[string]struct {
				OS      string         `toml:"os"`
				Address string         `toml:"address"`
				Connect bool           `toml:"connect"`
				Tools   []string       `toml:"tools"`
				Spec    map[string]any `toml:"spec"`
			} `toml:"machines"`
		} `toml:"providers"`
	}
	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	order, err := keyOrder(data)
	if err != nil {
		return nil, err
	}
	var out []DocProvider
	for _, pn := range childOrder(order, "providers") {
		p := raw.Providers[pn]
		dp := DocProvider{Name: pn, Kind: p.Kind}
		for _, mn := range childOrder(order, "providers", pn, "machines") {
			m := p.Machines[mn]
			dp.Machines = append(dp.Machines, DocMachine{Provider: pn, Name: mn, OS: m.OS, Address: m.Address,
				Connect: m.Connect, Tools: m.Tools, Spec: m.Spec})
		}
		out = append(out, dp)
	}
	return out, nil
}
