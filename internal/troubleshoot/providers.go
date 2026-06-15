package troubleshoot

import (
	"os"
	"path/filepath"
	"strings"
)

// DiagSource is a named set of on-disk paths belonging to a downstream MCP
// provider (e.g. Goose) that are useful when diagnosing a problem. Each entry
// is a candidate path or glob; only the ones that actually exist are collected,
// so the same registry works across OS layouts and provider versions without
// failing when a path is absent.
type DiagSource struct {
	// Label is the subdirectory the matched files land under inside the
	// bundle (providers/<provider>/<label>/...).
	Label string
	// Roots are candidate absolute paths. A root may be a concrete file, a
	// directory (walked recursively), or a glob pattern (filepath.Match
	// syntax). Tilde and environment variables are expanded before matching.
	Roots []string
}

// providerRegistry is the built-in, declarative map of provider name -> the
// diagnostic sources we know how to find. It is intentionally generous: extra
// candidate paths cost nothing because non-existent ones are skipped. Keeping
// this here (rather than in provider runtime code) honours the rule that
// Murtaugh stays above the ACP boundary — this is read-only diagnostics, never
// part of the request path.
func providerRegistry(home, goos string) map[string][]DiagSource {
	// GOOSE_PATH_ROOT, when set, relocates Goose's entire state tree; honour
	// it first so a non-default install is still captured.
	gooseRoots := func(rel ...string) []string {
		var out []string
		if root := strings.TrimSpace(os.Getenv("GOOSE_PATH_ROOT")); root != "" {
			for _, r := range rel {
				out = append(out, filepath.Join(root, r))
			}
		}
		return out
	}

	goose := []DiagSource{
		{
			Label: "sessions",
			Roots: append(gooseRoots("sessions"),
				filepath.Join(home, ".local", "share", "goose", "sessions", "sessions.db"),
				filepath.Join(home, ".local", "share", "goose", "sessions", "sessions.db-wal"),
				filepath.Join(home, ".local", "share", "goose", "sessions", "sessions.db-shm"),
				// macOS Application Support layout (some Goose builds).
				filepath.Join(home, "Library", "Application Support", "goose", "sessions", "sessions.db"),
				filepath.Join(home, "Library", "Application Support", "goose", "sessions", "sessions.db-wal"),
				filepath.Join(home, "Library", "Application Support", "goose", "sessions", "sessions.db-shm"),
			),
		},
		{
			Label: "logs",
			Roots: append(gooseRoots("logs"),
				filepath.Join(home, ".local", "state", "goose", "logs"),
				filepath.Join(home, ".config", "goose", "logs"),
				filepath.Join(home, ".local", "share", "goose", "logs"),
				filepath.Join(home, "Library", "Logs", "goose"),
			),
		},
		{
			Label: "config",
			Roots: append(gooseRoots("config.yaml"),
				// config.yaml is mode 0644 and references providers, not raw
				// keys; still passed through redaction as a text file.
				filepath.Join(home, ".config", "goose", "config.yaml"),
			),
		},
	}

	return map[string][]DiagSource{
		"goose": goose,
	}
}

// KnownProviders lists the provider names the bundler can collect diagnostics
// for. Used to validate the --include argument and document the tool.
func KnownProviders() []string {
	return []string{"goose"}
}

// resolveProviderSources expands a provider's candidate roots into the concrete
// files that exist on this machine. Globs are matched; directories are walked;
// missing paths are silently dropped. The returned map is label -> existing
// file paths.
func resolveProviderSources(provider, home, goos string) map[string][]string {
	sources, ok := providerRegistry(home, goos)[provider]
	if !ok {
		return nil
	}
	out := make(map[string][]string)
	for _, src := range sources {
		var files []string
		for _, root := range src.Roots {
			files = append(files, expandRoot(root)...)
		}
		if len(files) > 0 {
			out[src.Label] = dedupeStrings(files)
		}
	}
	return out
}

// expandRoot turns a single candidate root into the list of existing regular
// files it covers: a glob is matched, a directory is walked recursively, and a
// plain file is returned as-is. Anything that does not resolve to an existing
// regular file yields nothing.
func expandRoot(root string) []string {
	root = expandPath(root)
	if strings.ContainsAny(root, "*?[") {
		matches, err := filepath.Glob(root)
		if err != nil {
			return nil
		}
		var out []string
		for _, m := range matches {
			out = append(out, expandRoot(m)...)
		}
		return out
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil
	}
	if info.IsDir() {
		var out []string
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			out = append(out, p)
			return nil
		})
		return out
	}
	return []string{root}
}

// expandPath expands a leading ~ and any $ENV references in a path.
func expandPath(p string) string {
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return os.ExpandEnv(p)
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := in[:0]
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
