package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

// decodeFile decodes a TOML file strictly into v. Syntax errors, type errors, and
// unknown keys give the file, line, and column.
func decodeFile(path string, v any) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return data, decodeData(path, data, v)
}

// decodeData decodes TOML data into v (only the keys in data change); name is
// the source in error messages.
func decodeData(name string, data []byte, v any) error {
	if err := checkDurations(name, data, reflect.TypeOf(v)); err != nil {
		return err
	}
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return positioned(name, err)
	}
	return nil
}

var typeErrRE = regexp.MustCompile(`^cannot decode TOML (\w+) into .* of type (\S+)$`)

func typeName(goType string) string {
	switch {
	case strings.HasSuffix(goType, "Duration"):
		return "a duration string such as \"10m\""
	case goType == "string":
		return "a string"
	case goType == "int" || goType == "*int":
		return "an integer"
	case goType == "bool":
		return "a boolean"
	case strings.HasPrefix(goType, "[]"):
		return "an array"
	default:
		return "a table"
	}
}

func positioned(path string, err error) error {
	var strict *toml.StrictMissingError
	if errors.As(err, &strict) {
		errs := make([]error, 0, len(strict.Errors))
		for _, e := range strict.Errors {
			row, col := e.Position()
			errs = append(errs, fmt.Errorf("%s:%d:%d: unknown key %q", path, row, col, strings.Join(e.Key(), ".")))
		}
		return errors.Join(errs...)
	}
	var de *toml.DecodeError
	if errors.As(err, &de) {
		row, col := de.Position()
		msg := strings.TrimPrefix(de.Error(), "toml: ")
		if m := typeErrRE.FindStringSubmatch(msg); m != nil {
			msg = fmt.Sprintf("wrong type: got a TOML %s, want %s", m[1], typeName(m[2]))
		}
		if key := de.Key(); len(key) > 0 {
			return fmt.Errorf("%s:%d:%d: %s: %s", path, row, col, strings.Join(key, "."), msg)
		}
		return fmt.Errorf("%s:%d:%d: %s", path, row, col, msg)
	}
	return fmt.Errorf("%s: %w", path, err)
}

// keyOrder returns each distinct key path in the document, in the order of its
// first appearance. Go maps lose the file order, but the scheduler needs it
// (providers and machines in file order).
func keyOrder(data []byte) ([][]string, error) {
	var out [][]string
	seen := map[string]bool{}
	add := func(path []string) {
		k := strings.Join(path, "\x00")
		if !seen[k] {
			seen[k] = true
			out = append(out, append([]string(nil), path...))
		}
	}
	var walkValue func(prefix []string, n *unstable.Node)
	walkKeyValue := func(prefix []string, n *unstable.Node) {
		path := append([]string(nil), prefix...)
		it := n.Key()
		for it.Next() {
			path = append(path, string(it.Node().Data))
			add(path)
		}
		walkValue(path, n.Value())
	}
	walkValue = func(prefix []string, n *unstable.Node) {
		if n.Kind != unstable.InlineTable {
			return
		}
		it := n.Children()
		for it.Next() {
			walkKeyValue(prefix, it.Node())
		}
	}
	p := unstable.Parser{}
	p.Reset(data)
	var table []string
	for p.NextExpression() {
		e := p.Expression()
		switch e.Kind {
		case unstable.Table, unstable.ArrayTable:
			table = table[:0]
			it := e.Key()
			for it.Next() {
				table = append(table, string(it.Node().Data))
				add(table)
			}
		case unstable.KeyValue:
			walkKeyValue(table, e)
		}
	}
	return out, p.Error()
}

// childOrder returns the names below prefix (one level deeper), in file order.
func childOrder(order [][]string, prefix ...string) []string {
	var names []string
	for _, path := range order {
		if len(path) != len(prefix)+1 {
			continue
		}
		match := true
		for i := range prefix {
			if path[i] != prefix[i] {
				match = false
				break
			}
		}
		if match {
			names = append(names, path[len(prefix)])
		}
	}
	return names
}

// checkDurations reports a number at a duration key with its position. go-toml
// passes a number to Duration.UnmarshalText without a position, so the owner
// would not see which key is wrong.
func checkDurations(file string, data []byte, t reflect.Type) error {
	p := unstable.Parser{}
	p.Reset(data)
	var errs []error
	var table []string
	var walk func(prefix []string, kv *unstable.Node)
	walk = func(prefix []string, kv *unstable.Node) {
		path := append([]string(nil), prefix...)
		it := kv.Key()
		for it.Next() {
			path = append(path, string(it.Node().Data))
		}
		v := kv.Value()
		switch v.Kind {
		case unstable.Integer, unstable.Float:
			if isDurationPath(t, path) {
				pos := p.Shape(v.Raw).Start
				errs = append(errs, fmt.Errorf("%s:%d:%d: %s: wrong type: got a number, want a duration string such as \"10m\"",
					file, pos.Line, pos.Column, strings.Join(path, ".")))
			}
		case unstable.InlineTable:
			children := v.Children()
			for children.Next() {
				walk(path, children.Node())
			}
		}
	}
	for p.NextExpression() {
		e := p.Expression()
		switch e.Kind {
		case unstable.Table, unstable.ArrayTable:
			table = table[:0]
			it := e.Key()
			for it.Next() {
				table = append(table, string(it.Node().Data))
			}
		case unstable.KeyValue:
			walk(table, e)
		}
	}
	if p.Error() != nil {
		return nil // the decoder reports syntax errors with their position
	}
	return errors.Join(errs...)
}

func isDurationPath(t reflect.Type, path []string) bool {
	for _, seg := range path {
		for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
			t = t.Elem()
		}
		switch t.Kind() {
		case reflect.Map:
			t = t.Elem()
		case reflect.Struct:
			if t == durationType {
				return false
			}
			found := false
			for i := range t.NumField() {
				name, _, _ := strings.Cut(t.Field(i).Tag.Get("toml"), ",")
				if name == seg {
					t, found = t.Field(i).Type, true
					break
				}
			}
			if !found {
				return false
			}
		default:
			return false
		}
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t == durationType
}
