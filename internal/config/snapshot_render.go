package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

// This file renders a config Snapshot as YAML and diffs two renderings. It
// exists for the change-approval flow: before a running daemon adopts an edited
// configuration, the admin is shown what actually changed.
//
// YAML rather than the JSON the store holds, and rather than Go's struct dump,
// because the person approving is reading to decide — "did someone just widen
// allowed_users" is a question about meaning, and YAML is the shape they
// already know this configuration in.

// RenderSnapshotYAML renders a snapshot as a deterministic YAML document.
//
// Determinism is the whole requirement. The rendering is diffed against another
// rendering, so any instability — map iteration order, key order inside a
// body, indentation drift — shows up as a spurious change and trains the admin
// to approve without reading. Sections and names are sorted, and each JSON body
// is decoded and re-encoded through the same marshaller so two equal
// configurations always produce byte-identical output.
func RenderSnapshotYAML(snap Snapshot) ([]byte, error) {
	doc := map[string]any{}

	bySection := map[string]map[string]any{}
	for _, item := range snap.Items {
		value, err := decodeForRender(item.Body)
		if err != nil {
			return nil, fmt.Errorf("render %s/%s: %w", item.Section, item.Name, err)
		}
		if bySection[item.Section] == nil {
			bySection[item.Section] = map[string]any{}
		}
		bySection[item.Section][item.Name] = value
	}
	for section, items := range bySection {
		doc[section] = items
	}

	for _, single := range snap.Singletons {
		value, err := decodeForRender(single.Body)
		if err != nil {
			return nil, fmt.Errorf("render singleton %s: %w", single.Key, err)
		}
		// An empty singleton is the ABSENCE of configuration, not a value:
		// AssembleFromRows leaves an absent key at its zero value, so a stored
		// `{}` and a missing row produce the identical running config.
		//
		// Rendering them the same is what makes a rollback stable. Reverting
		// writes `{}` where the approved snapshot had no row at all (the Store
		// has no singleton delete, and adding one for a single caller would be
		// worse), so rendering `{}` as present would make the daemon see its own
		// rollback as a fresh change and prompt for it — forever.
		if isEmptyValue(value) {
			continue
		}
		doc[single.Key] = value
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	// yaml.v3 sorts map keys, so a Go map is rendered in a stable order without
	// any help here — which is why the document is assembled as plain maps
	// rather than as an ordered node tree.
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("render config as YAML: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("render config as YAML: %w", err)
	}
	return buf.Bytes(), nil
}

// decodeForRender turns a stored JSON body into plain Go values for YAML
// encoding. Decoding into `any` rather than the typed struct is deliberate: the
// diff should show what is actually STORED, including a field the running
// binary does not know about, rather than what this version's structs happen to
// model. A config written by a newer Murtaugh must still be reviewable by an
// older one.
func decodeForRender(body json.RawMessage) (any, error) {
	if len(body) == 0 {
		return nil, nil
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, fmt.Errorf("decode stored body: %w", err)
	}
	return value, nil
}

// isEmptyValue reports whether a decoded body carries no configuration: JSON
// null, or an object with no keys.
func isEmptyValue(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case map[string]any:
		return len(v) == 0
	default:
		return false
	}
}

// SnapshotChanged reports whether two snapshots differ in content.
//
// It compares renderings rather than the raw bodies, so a store that re-encoded
// a body without changing its meaning — a different key order, a re-serialised
// number — does not read as a change. Waking an admin for a no-op edit is how a
// review prompt becomes noise.
func SnapshotChanged(before, after Snapshot) (bool, error) {
	a, err := RenderSnapshotYAML(before)
	if err != nil {
		return false, err
	}
	b, err := RenderSnapshotYAML(after)
	if err != nil {
		return false, err
	}
	return !bytes.Equal(a, b), nil
}

// DiffSnapshots renders a diff between two snapshots for an admin to read. An
// empty result means they are equivalent.
//
// It emits NO file or hunk headers. A unified diff's `--- a/file` and `@@ -1,7`
// lines orient you in a file you can go and open; this YAML has no file — it is
// a rendering of database rows — so those lines would name a path that does not
// exist and offset arithmetic against a document nobody can look up. What is
// left is the part that carries meaning: the changed lines and their context.
func DiffSnapshots(before, after Snapshot, context int) (string, error) {
	a, err := RenderSnapshotYAML(before)
	if err != nil {
		return "", err
	}
	b, err := RenderSnapshotYAML(after)
	if err != nil {
		return "", err
	}
	return PlainDiff(string(a), string(b), context), nil
}

// PlainDiff is UnifiedDiff without the hunk headers.
//
// Separate hunks are joined by a blank line. Without headers there is nothing
// to say "and now we are 40 lines further down", so the blank line is the only
// signal that the listing jumped — enough to stop two distant edits reading as
// one contiguous block, and quieter than inventing a marker.
func PlainDiff(before, after string, context int) string {
	return renderDiff(before, after, context, false)
}

// UnifiedDiff produces a unified diff of two texts with the given number of
// context lines, including @@ hunk headers.
//
// Hand-rolled because the alternative is a dependency for one screen of code.
// It is line-based (config YAML is line-oriented) and emits the standard
// +/-/space prefixes, so it reads like every other diff an operator has seen.
func UnifiedDiff(before, after string, context int) string {
	return renderDiff(before, after, context, true)
}

// renderDiff is the shared body of PlainDiff and UnifiedDiff.
func renderDiff(before, after string, context int, headers bool) string {
	if context < 0 {
		context = 0
	}
	a, b := splitLines(before), splitLines(after)
	ops := diffOps(a, b)

	// Mark which lines belong to a hunk: every changed line plus `context`
	// lines either side.
	keep := make([]bool, len(ops))
	for i, op := range ops {
		if op.kind == opEqual {
			continue
		}
		for j := max(0, i-context); j <= min(len(ops)-1, i+context); j++ {
			keep[j] = true
		}
	}

	var out bytes.Buffer
	wrote := false
	for i := 0; i < len(ops); {
		if !keep[i] {
			i++
			continue
		}
		start := i
		for i < len(ops) && keep[i] {
			i++
		}
		if wrote && !headers {
			out.WriteByte('\n')
		}
		writeHunk(&out, ops[start:i], headers)
		wrote = true
	}
	return out.String()
}

// writeHunk emits one hunk, with its @@ header only when headers is set.
func writeHunk(out *bytes.Buffer, ops []diffOp, headers bool) {
	if headers {
		var aStart, bStart, aCount, bCount int
		for i, op := range ops {
			if i == 0 {
				aStart, bStart = op.aLine, op.bLine
			}
			switch op.kind {
			case opEqual:
				aCount++
				bCount++
			case opDelete:
				aCount++
			case opInsert:
				bCount++
			}
		}
		fmt.Fprintf(out, "@@ -%d,%d +%d,%d @@\n", aStart+1, aCount, bStart+1, bCount)
	}
	for _, op := range ops {
		switch op.kind {
		case opEqual:
			fmt.Fprintf(out, " %s\n", op.text)
		case opDelete:
			fmt.Fprintf(out, "-%s\n", op.text)
		case opInsert:
			fmt.Fprintf(out, "+%s\n", op.text)
		}
	}
}

type diffKind int

const (
	opEqual diffKind = iota
	opDelete
	opInsert
)

type diffOp struct {
	kind  diffKind
	text  string
	aLine int
	bLine int
}

// diffOps computes a line diff via the classic LCS table.
//
// Config renderings are on the order of hundreds of lines, so the O(n*m) table
// is a few hundred kilobytes at worst — far below the cost of pulling in a diff
// library, and simple enough to be obviously correct.
func diffOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
				continue
			}
			lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
		}
	}

	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{kind: opEqual, text: a[i], aLine: i, bLine: j})
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{kind: opDelete, text: a[i], aLine: i, bLine: j})
			i++
		default:
			ops = append(ops, diffOp{kind: opInsert, text: b[j], aLine: i, bLine: j})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{kind: opDelete, text: a[i], aLine: i, bLine: j})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{kind: opInsert, text: b[j], aLine: i, bLine: j})
	}
	return ops
}

// splitLines splits text into lines, dropping a single trailing newline so a
// well-formed document does not diff against a phantom empty last line.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	if s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
	}
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	return append(lines, s[start:])
}

// SortSnapshot orders a snapshot's rows canonically, so two snapshots of equal
// content are equal values as well as equal renderings.
func SortSnapshot(snap Snapshot) Snapshot {
	items := append([]SnapshotItem(nil), snap.Items...)
	sort.Slice(items, func(i, j int) bool {
		if items[i].Section != items[j].Section {
			return items[i].Section < items[j].Section
		}
		return items[i].Name < items[j].Name
	})
	singles := append([]SnapshotSingleton(nil), snap.Singletons...)
	sort.Slice(singles, func(i, j int) bool { return singles[i].Key < singles[j].Key })
	return Snapshot{Items: items, Singletons: singles}
}
