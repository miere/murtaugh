package assets

import "embed"

// FS contains reference Slack assets that are also used as built-in defaults:
// the seed config files, the Block Kit templates under templates/, and the
// bundled agent skills under skills/ (each a SKILL.md + reference/ + examples/
// tree). Both templates and skills are embedded recursively.
//
//go:embed slack.yaml agents.yaml jobs.yaml templates skills
var FS embed.FS
