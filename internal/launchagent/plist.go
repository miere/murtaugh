package launchagent

import (
	"bytes"
	"fmt"
	"path/filepath"
	"text/template"
)

// The PATH block is there because a launchd job inherits almost nothing, and an
// agent backend is usually a binary on the operator's shell PATH.
const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
  <dict>
    <key>Label</key>
    <string>{{.Label}}</string>
    <key>ProgramArguments</key>
    <array>
{{- range .Arguments}}
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
    <string>{{.OutLog}}</string>
    <key>StandardErrorPath</key>
    <string>{{.ErrLog}}</string>
    <key>EnvironmentVariables</key>
    <dict>
      <key>PATH</key>
      <string>{{.Home}}/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    </dict>
  </dict>
</plist>
`

func render(label string, spec Spec, logsDir string) ([]byte, error) {
	tmpl, err := template.New("plist").Parse(plistTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse plist template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct {
		Label     string
		Arguments []string
		Home      string
		OutLog    string
		ErrLog    string
	}{
		Label:     label,
		Arguments: append([]string{spec.BinaryPath}, spec.Arguments...),
		Home:      spec.Home,
		OutLog:    filepath.Join(logsDir, label+".out.log"),
		ErrLog:    filepath.Join(logsDir, label+".err.log"),
	}); err != nil {
		return nil, fmt.Errorf("render plist: %w", err)
	}
	return buf.Bytes(), nil
}
