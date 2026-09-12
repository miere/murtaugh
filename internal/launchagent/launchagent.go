// Package launchagent writes the macOS LaunchAgent a Murtaugh binary runs
// itself under. Both binaries share it so a gateway and a node are installed
// by one implementation.
package launchagent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/miere/murtaugh/internal/config"
)

// DefaultAlias names the LaunchAgent when the operator does not, so the common
// single-gateway, single-node machine needs no flag.
const DefaultAlias = "default"

// Spec describes one LaunchAgent: which binary runs, under which role and
// alias, and with which arguments.
type Spec struct {
	Role       config.Role
	Alias      string
	BinaryPath string
	Arguments  []string
	// Home is the login home the job runs from; LaunchAgentsDir and LogsDir
	// default to the usual places beneath it.
	Home            string
	LaunchAgentsDir string
	LogsDir         string
}

// Result reports where the plist landed and under which label.
type Result struct {
	Path    string `json:"path"`
	Label   string `json:"label"`
	Created bool   `json:"created"`
}

// String renders the CLI confirmation.
func (r Result) String() string {
	verb := "updated"
	if r.Created {
		verb = "wrote"
	}
	return fmt.Sprintf("%s %s (%s). Load it with `launchctl bootstrap gui/$(id -u) %s`.", verb, r.Path, r.Label, r.Path)
}

// Label is the LaunchAgent label for a role and alias: murtaugh.gateway.<alias>
// or murtaugh.node.<alias>. The alias is what lets two nodes share a machine.
func Label(role config.Role, alias string) (string, error) {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		alias = DefaultAlias
	}
	if strings.ContainsAny(alias, "/ \t") {
		return "", fmt.Errorf("alias %q must not contain spaces or slashes: it is part of the LaunchAgent label", alias)
	}
	switch role {
	case config.RoleGateway:
		return "murtaugh.gateway." + alias, nil
	case config.RoleNode:
		return "murtaugh.node." + alias, nil
	default:
		return "", fmt.Errorf("no LaunchAgent label for role %q", role)
	}
}

// Write renders the plist for spec and writes it. An existing plist is refused
// unless updateExisting is set: overwriting the file that runs somebody's live
// daemon is not something to do on the way past.
func Write(spec Spec, updateExisting bool) (Result, error) {
	if strings.TrimSpace(spec.BinaryPath) == "" {
		return Result{}, errors.New("binary_path is required: name the binary this LaunchAgent should run")
	}
	label, err := Label(spec.Role, spec.Alias)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(spec.Home) == "" {
		return Result{}, errors.New("no home directory resolved, so there is nowhere to write the LaunchAgent")
	}
	agentsDir := spec.LaunchAgentsDir
	if strings.TrimSpace(agentsDir) == "" {
		agentsDir = filepath.Join(spec.Home, "Library", "LaunchAgents")
	}
	logsDir := spec.LogsDir
	if strings.TrimSpace(logsDir) == "" {
		logsDir = filepath.Join(spec.Home, "Library", "Logs", "murtaugh")
	}
	for _, dir := range []string{agentsDir, logsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return Result{}, fmt.Errorf("ensure %q: %w", dir, err)
		}
	}

	path := filepath.Join(agentsDir, label+".plist")
	existed := pathExists(path)
	if existed && !updateExisting {
		return Result{}, fmt.Errorf("%s already exists (label %s): pass --update-existing to replace it. "+
			"It may be running a live daemon, so this command will not overwrite one by accident", path, label)
	}

	body, err := render(label, spec, logsDir)
	if err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return Result{}, fmt.Errorf("write %q: %w", path, err)
	}
	return Result{Path: path, Label: label, Created: !existed}, nil
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
