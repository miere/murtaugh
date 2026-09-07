package auth

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// versionProbeTimeout bounds the probe. It runs on a path that is already
// failing, so a wedged CLI must not turn a bad diagnostic into a worse hang.
const versionProbeTimeout = 5 * time.Second

// VersionDrift reports, in one clause fit for a failure card, whether the
// installed CLI is a different release from the one this profile's output shape
// was verified against. It returns "" when they agree, when the profile carries
// no probe, or when the probe itself cannot answer.
//
// This is a diagnostic, never a gate. A drifted version is not proof that the
// version is why the flow failed, and refusing to sign in because a CLI updated
// would lock the operator out for the sake of a guess. It runs only on the
// failure path and only ever adds a sentence to what the admin already sees.
func (p Profile) VersionDrift(ctx context.Context) string {
	installed, ok := p.installedVersion(ctx)
	if !ok || p.VerifiedVersion == "" || installed == p.VerifiedVersion {
		return ""
	}
	return fmt.Sprintf("the %s CLI reports version %s, but this flow was verified against %s — its output shape may have changed",
		p.Command, installed, p.VerifiedVersion)
}

// installedVersion asks the CLI what it is.
//
// Only the first whitespace-delimited field is kept: version output habitually
// trails a product name or a build stamp (`2.1.260 (Claude Code)`), and none of
// that is the number being compared.
func (p Profile) installedVersion(ctx context.Context) (string, bool) {
	if len(p.versionProbe) == 0 || strings.TrimSpace(p.Command) == "" {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, versionProbeTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, p.Command, p.versionProbe...).Output()
	if err != nil {
		return "", false
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", false
	}
	return fields[0], true
}
