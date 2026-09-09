package launchd

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

// Role is which daemon a plist starts.
//
// #170 puts a gateway and a runtime node in SEPARATE PROCESSES, so a box
// running both runs two LaunchAgents — distinct labels, distinct log files,
// separate configuration roots — and a crash-looping runtime node does not take
// Slack down with it. One plist with a switch in it would have given them a
// shared lifetime, which is the property being avoided.
type Role string

const (
	// RoleGateway is the Slack-facing daemon and the DEFAULT, because it is the
	// one this label has always started. Its label, its arguments and its log
	// filenames are named in AGENTS.md, docs/operations.md, cli-help.md, the
	// murtaugh-setup skill and every runbook, and every existing install has a
	// job registered under it. Changing any of them is a migration, not an
	// improvement.
	RoleGateway Role = "gateway"
	// RoleRuntime is the runtime node: an always-on daemon, because scheduled
	// jobs must fire when their owner is not chatting, and a node that is asleep
	// simply does not run them.
	RoleRuntime Role = "runtime"
)

// spec is everything that differs between the two plists. It is a table rather
// than three ifs in the renderer so that "what does a role change" is one thing
// to read, and so a third role cannot be added by editing only two of the three.
type spec struct {
	// Label is the launchd job name, and it is also the plist's FILENAME and
	// what `launchctl kickstart gui/<uid>/<label>` names. Two jobs must never
	// share one.
	Label string
	// Args is what launchd execs, after the binary path.
	Args []string
	// LogStem names the pair of log files. Two daemons must not share them: the
	// troubleshoot bundle and every runbook read slack.{out,err}.log, and a
	// second daemon appending to those interleaves with the gateway's own.
	LogStem string
	// Binary is the default filename installed beside the CLI, used only to
	// describe the role — the caller always supplies the real path.
	Binary string
}

func specFor(role Role) (spec, error) {
	switch role {
	case RoleGateway, "":
		return spec{
			Label:   "dev.murtaugh",
			Args:    []string{"slack", "gateway"},
			LogStem: "slack",
			Binary:  "murtaugh",
		}, nil
	case RoleRuntime:
		return spec{
			Label: "dev.murtaugh.runtime",
			// No arguments. murtaugh-runtime reads its own configuration root
			// (~/.config/murtaugh/node) by default, so a plist that named one
			// would be a second place the path is written down.
			Args:    nil,
			LogStem: "runtime",
			Binary:  "murtaugh-runtime",
		}, nil
	default:
		return spec{}, fmt.Errorf("unknown role %q: expected %q or %q", role, RoleGateway, RoleRuntime)
	}
}

// plistTemplate matches the layout install.sh emitted, including the PATH
// environment variable so a launchd-launched daemon resolves the same
// dependencies an interactive shell does.
//
// KeepAlive is true for BOTH roles and is load-bearing for the runtime one:
// item 12's zero-profile onboarding has a node write its new configuration,
// answer the gateway, and then exit, relying on the supervisor to bring it back
// into that configuration. Without KeepAlive a freshly onboarded node simply
// dies.
const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
  <dict>
    <key>Label</key>
    <string>{{.Label}}</string>
    <key>ProgramArguments</key>
    <array>
      <string>{{.Binary}}</string>
{{- range .Args}}
      <string>{{.}}</string>
{{- end}}
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>WorkingDirectory</key>
    <string>{{.Home}}</string>
    <key>StandardOutPath</key>
    <string>{{.LogsDir}}/{{.LogStem}}.out.log</string>
    <key>StandardErrorPath</key>
    <string>{{.LogsDir}}/{{.LogStem}}.err.log</string>
    <key>EnvironmentVariables</key>
    <dict>
      <key>PATH</key>
      <string>{{.Home}}/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    </dict>
  </dict>
</plist>
`

// renderPlist substitutes the role's label, arguments and log stem alongside
// Binary, Home and LogsDir.
func renderPlist(role Role, binary, home, logsDir string) ([]byte, error) {
	s, err := specFor(role)
	if err != nil {
		return nil, err
	}
	tmpl, err := template.New("plist").Parse(plistTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse plist template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct {
		Label   string
		Binary  string
		Args    []string
		Home    string
		LogsDir string
		LogStem string
	}{Label: s.Label, Binary: binary, Args: s.Args, Home: home, LogsDir: logsDir, LogStem: s.LogStem}); err != nil {
		return nil, fmt.Errorf("render plist: %w", err)
	}
	return buf.Bytes(), nil
}

// roleFrom reads the role argument, defaulting to the gateway.
func roleFrom(v string) (Role, error) {
	role := Role(strings.TrimSpace(v))
	if role == "" {
		role = RoleGateway
	}
	if _, err := specFor(role); err != nil {
		return "", err
	}
	return role, nil
}
