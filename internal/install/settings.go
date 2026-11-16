package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"
)

// Settings edits are recorded in a manifest so uninstall removes exactly
// what Iris added and restores any scalar it overwrote.

type Manifest struct {
	SettingsPath string          `json:"settingsPath"`
	InstalledAt  time.Time       `json:"installedAt"`
	Scalars      []ScalarChange  `json:"scalars"`
	ArrayAdds    []ArrayAddition `json:"arrayAdds"`
	LaunchAgent  string          `json:"launchAgent,omitempty"`
}

type ScalarChange struct {
	Path    string          `json:"path"` // dotted, e.g. sandbox.enabled or env.KUBECONFIG
	Had     bool            `json:"had"`
	Before  json.RawMessage `json:"before,omitempty"`
	Applied json.RawMessage `json:"applied"`
}

type ArrayAddition struct {
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

type Doc map[string]any

func ReadSettings(path string) (Doc, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Doc{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return Doc{}, nil
	}
	var d Doc
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON (fix it first): %w", path, err)
	}
	return d, nil
}

func WriteSettings(path string, d Doc) error {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".iris-tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// splitPath splits on dots; env var names and the like never contain dots.
func splitPath(p string) []string { return strings.Split(p, ".") }

func (d Doc) get(path string) (any, bool) {
	var cur any = map[string]any(d)
	for _, k := range splitPath(path) {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[k]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func (d Doc) parent(path string, create bool) (map[string]any, string, error) {
	parts := splitPath(path)
	cur := map[string]any(d)
	for _, k := range parts[:len(parts)-1] {
		next, ok := cur[k]
		if !ok {
			if !create {
				return nil, "", nil
			}
			m := map[string]any{}
			cur[k] = m
			cur = m
			continue
		}
		m, ok := next.(map[string]any)
		if !ok {
			return nil, "", fmt.Errorf("settings key %s is not an object", k)
		}
		cur = m
	}
	return cur, parts[len(parts)-1], nil
}

// Editor applies changes and records them.
type Editor struct {
	D Doc
	M *Manifest
}

func toRaw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// norm round-trips through JSON so Go values compare equal to decoded ones.
func norm(v any) any {
	var out any
	_ = json.Unmarshal(toRaw(v), &out)
	return out
}

func (e *Editor) Set(path string, v any) error {
	p, k, err := e.D.parent(path, true)
	if err != nil {
		return err
	}
	before, had := p[k]
	if had && reflect.DeepEqual(before, norm(v)) {
		return nil // already as wanted; nothing to undo later
	}
	sc := ScalarChange{Path: path, Had: had, Applied: toRaw(v)}
	if had {
		sc.Before = toRaw(before)
	}
	p[k] = norm(v)
	e.M.Scalars = append(e.M.Scalars, sc)
	return nil
}

func (e *Editor) Append(path string, v any) error {
	p, k, err := e.D.parent(path, true)
	if err != nil {
		return err
	}
	nv := norm(v)
	arr, _ := p[k].([]any)
	if p[k] != nil && arr == nil {
		return fmt.Errorf("settings key %s is not an array", path)
	}
	for _, x := range arr {
		if reflect.DeepEqual(x, nv) {
			return nil
		}
	}
	p[k] = append(arr, nv)
	e.M.ArrayAdds = append(e.M.ArrayAdds, ArrayAddition{Path: path, Value: toRaw(v)})
	return nil
}

// Revert undoes a manifest. Scalars are restored only if still holding the
// value Iris applied (the user may have changed them since).
func Revert(d Doc, m *Manifest) []string {
	var notes []string
	for i := len(m.ArrayAdds) - 1; i >= 0; i-- {
		a := m.ArrayAdds[i]
		p, k, _ := d.parent(a.Path, false)
		if p == nil {
			continue
		}
		arr, _ := p[k].([]any)
		var want any
		_ = json.Unmarshal(a.Value, &want)
		out := arr[:0:0]
		for _, x := range arr {
			if !reflect.DeepEqual(x, want) {
				out = append(out, x)
			}
		}
		if len(out) == 0 {
			delete(p, k)
		} else {
			p[k] = out
		}
	}
	for i := len(m.Scalars) - 1; i >= 0; i-- {
		s := m.Scalars[i]
		p, k, _ := d.parent(s.Path, false)
		if p == nil {
			continue
		}
		var applied any
		_ = json.Unmarshal(s.Applied, &applied)
		if cur, ok := p[k]; !ok || !reflect.DeepEqual(cur, applied) {
			notes = append(notes, fmt.Sprintf("left %s alone (changed since install)", s.Path))
			continue
		}
		if s.Had {
			var before any
			_ = json.Unmarshal(s.Before, &before)
			p[k] = before
		} else {
			delete(p, k)
		}
	}
	pruneEmpty(map[string]any(d))
	return notes
}

func pruneEmpty(m map[string]any) {
	for k, v := range m {
		if sub, ok := v.(map[string]any); ok {
			pruneEmpty(sub)
			if len(sub) == 0 {
				delete(m, k)
			}
		}
	}
}

func LoadManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	return &m, json.Unmarshal(b, &m)
}

func SaveManifest(path string, m *Manifest) error {
	b, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(path, b, 0o600)
}
