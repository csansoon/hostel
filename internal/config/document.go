package config

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/pelletier/go-toml/v2/unstable"
)

// documentKeys are the top-level keys of the configuration document, the part of
// the server configuration in the database since M9 (multiplayer.md, section
// 7). The other keys stay in server.toml: the server needs them before it opens
// its database, or they belong to the server machine.
var documentKeys = map[string]bool{
	"providers": true, "idle_timeout": true, "aging": true, "poll_timeout": true,
	"limit_retry": true, "port_process_offline": true, "intervals": true, "mise": true,
}

// IsDocumentKey reports whether a top-level key belongs to the document.
func IsDocumentKey(key string) bool { return documentKeys[key] }

// Document is a revision of the configuration document.
type Document struct {
	Revision int
	Data     []byte
	// Tools are the server's defined tools (name → file → content); nil:
	// the default tools.
	Tools map[string]map[string]string
	// ToolsDir has the files of Tools on disk (the server uploads them from
	// there), or "".
	ToolsDir string
}

// Name is the document's name in error messages.
func (d Document) Name() string { return fmt.Sprintf("configuration revision %d", d.Revision) }

// topKeys returns the top-level keys of a TOML document in file order.
func topKeys(data []byte) ([]string, error) {
	order, err := keyOrder(data)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, path := range order {
		if len(path) == 1 {
			out = append(out, path[0])
		}
	}
	return out, nil
}

// HasDocumentKeys reports whether server.toml data has keys of the document
// (a server.toml from before M9).
func HasDocumentKeys(data []byte) (bool, error) {
	keys, err := topKeys(data)
	if err != nil {
		return false, err
	}
	for _, k := range keys {
		if documentKeys[k] {
			return true, nil
		}
	}
	return false, nil
}

// SplitServerFile splits a server.toml from before M9 into its local part and
// the document. It keeps the text: each top-level key or table goes to one part
// with the comment lines directly above it, so the comments and the order of
// the providers and machines stay.
func SplitServerFile(data []byte) (local, doc []byte, err error) {
	lines := bytes.SplitAfter(data, []byte("\n"))
	lineOf := func(offset int) int {
		n := 0
		for i, l := range lines {
			if offset < n+len(l) {
				return i
			}
			n += len(l)
		}
		return len(lines) - 1
	}
	type part struct {
		line int
		doc  bool
	}
	var parts []part
	p := unstable.Parser{}
	p.Reset(data)
	inTable := false
	tableDoc := false
	for p.NextExpression() {
		e := p.Expression()
		var first *unstable.Node
		switch e.Kind {
		case unstable.Table, unstable.ArrayTable:
			it := e.Key()
			it.Next()
			first = it.Node()
			inTable = true
			tableDoc = documentKeys[string(first.Data)] || !localKeys[string(first.Data)]
			parts = append(parts, part{lineOf(int(p.Range(first.Data).Offset)), tableDoc})
		case unstable.KeyValue:
			if inTable {
				continue
			}
			it := e.Key()
			it.Next()
			first = it.Node()
			parts = append(parts, part{lineOf(int(p.Range(first.Data).Offset)), documentKeys[string(first.Data)]})
		}
	}
	if err := p.Error(); err != nil {
		return nil, nil, err
	}
	// A part starts at its first line, or earlier at the comment lines
	// directly above it; the lines before the first part go with it.
	starts := make([]int, len(parts))
	for i, pt := range parts {
		start := pt.line
		floor := 0
		if i > 0 {
			floor = parts[i-1].line + 1
		}
		for start > floor && strings.HasPrefix(strings.TrimSpace(string(lines[start-1])), "#") {
			start--
		}
		if i == 0 {
			start = 0
		}
		starts[i] = start
	}
	var l, d bytes.Buffer
	for i, pt := range parts {
		end := len(lines)
		if i+1 < len(parts) {
			end = starts[i+1]
		}
		dst := &l
		if pt.doc {
			dst = &d
		}
		for _, line := range lines[starts[i]:end] {
			dst.Write(line)
		}
	}
	return l.Bytes(), d.Bytes(), nil
}

// localKeys are the top-level keys of server.toml after M9.
var localKeys = map[string]bool{
	"server_id": true, "listen": true, "gateway_listen": true, "gateway_public": true,
	"tls": true, "public_urls": true, "data_dir": true, "helper_dir": true,
}
