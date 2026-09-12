// Package cmdline holds the global argument handling both Murtaugh binaries
// share: the --config and --json flags, and how a help request is recognised.
// One implementation so the two binaries cannot drift on what a flag means.
package cmdline

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ExtractConfigFlag pulls the global --config flag out of args, supporting
// both `--config=VALUE` and `--config VALUE` (and the single-dash variants).
// Unknown flags are passed through to the selected frontend untouched.
func ExtractConfigFlag(args []string, fallback string) (string, []string, error) {
	out := make([]string, 0, len(args))
	configPath := fallback
	seen := false
	set := func(value string) error {
		if seen {
			return fmt.Errorf("--config given twice (%q then %q): it is a GLOBAL flag naming the configuration this command runs against, and applies to the whole command line. "+
				"A destination path belongs to the subcommand's own flag, e.g. `cfg node split --dest`", configPath, value)
		}
		seen, configPath = true, value
		return nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, value, hasValue := parseToken(a)
		if name != "config" {
			out = append(out, a)
			continue
		}
		if hasValue {
			if err := set(value); err != nil {
				return "", nil, err
			}
			continue
		}
		if i+1 >= len(args) {
			return "", nil, errors.New("--config requires a value")
		}
		if err := set(args[i+1]); err != nil {
			return "", nil, err
		}
		i++
	}
	return configPath, out, nil
}

// ExtractJSONFlag pulls the global --json boolean out of args and returns
// whether it was set along with the remaining tokens. A bare `--json` enables
// it; the `--json=true` / `--json=false` form is also honoured. It is stripped
// before help/mode selection and tool dispatch because the tool flag parser
// rejects value-less flags. Single-dash `-json` is accepted to match the
// config flag's leniency.
func ExtractJSONFlag(args []string) (bool, []string, error) {
	out := make([]string, 0, len(args))
	enabled := false
	for _, a := range args {
		name, value, hasValue := parseToken(a)
		if name != "json" {
			out = append(out, a)
			continue
		}
		if !hasValue {
			enabled = true
			continue
		}
		b, err := strconv.ParseBool(value)
		if err != nil {
			return false, nil, fmt.Errorf("--json: expected boolean, got %q", value)
		}
		enabled = b
	}
	return enabled, out, nil
}

// HelpRequest reports whether args asks for help and, if so, returns the
// command tokens that scope it (empty means the full document). A leading
// `help` subcommand consumes the rest of the tokens as the command to look up;
// otherwise a `--help`/`-h` flag anywhere triggers help scoped to the
// surrounding command. The `--config` flag has already been stripped.
func HelpRequest(args []string) ([]string, bool) {
	if len(args) > 0 && args[0] == "help" {
		return args[1:], true
	}
	tokens := make([]string, 0, len(args))
	found := false
	for _, a := range args {
		if a == "--help" || a == "-h" {
			found = true
			continue
		}
		tokens = append(tokens, a)
	}
	if found {
		return tokens, true
	}
	return nil, false
}

// IsCommand reports whether args asks for a subcommand rather than for the
// daemon. A leading flag (or nothing at all) belongs to the daemon's own flag
// set; a bare word is a tool name.
func IsCommand(args []string) bool {
	return len(args) > 0 && !strings.HasPrefix(args[0], "-")
}

// parseToken inspects a token for the --name / -name flag form. It returns the
// bare flag name (without dashes), the embedded value when the token uses the
// --key=value form, and whether such a value was present. Tokens that do not
// look like a flag return name="".
func parseToken(a string) (string, string, bool) {
	if !strings.HasPrefix(a, "-") {
		return "", "", false
	}
	trimmed := strings.TrimLeft(a, "-")
	name, value, hasValue := trimmed, "", false
	if i := strings.IndexByte(trimmed, '='); i >= 0 {
		name = trimmed[:i]
		value = trimmed[i+1:]
		hasValue = true
	}
	return name, value, hasValue
}
