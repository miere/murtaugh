package assets

import "embed"

// FS contains reference Slack assets that are also used as built-in defaults.
//
//go:embed slack.yaml agents.yaml ping/*.json unfurl/*.json skills/*.md
var FS embed.FS
