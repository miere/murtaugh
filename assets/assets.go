package assets

import "embed"

// FS contains reference Slack assets that are also used as built-in defaults.
//
//go:embed slack.yaml ping/*.json
var FS embed.FS
