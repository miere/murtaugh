// Package auth describes the authentication workflows the auth-request tool
// knows how to drive, and how to read progress out of each one.
//
// A Profile answers four questions about a flow:
//
//   - what to run (an argv for internal/proc)
//   - whether it finishes by the user pasting a code back, or entirely in the
//     browser — which decides the card's button layout
//   - how to spot the verification URL in the child's output
//   - how to read success from the exit status
//
// The split matters because the two facets need different cards. A code flow
// shows "Enter Code" as the primary action and keeps the process parked on
// stdin until the code arrives. A browser-only flow shows "Open In Browser" as
// the primary action and simply waits for the child to exit.
package auth

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/miere/murtaugh/internal/proc"
)

// Profile is one authentication workflow.
type Profile struct {
	// Name is the identifier the agent passes to the tool.
	Name string

	// NeedsCode reports whether the flow completes by the user pasting a
	// verification code back into the process. False means the whole exchange
	// happens in the browser and the process exits on its own.
	NeedsCode bool

	// Command and Args are the argv handed to proc. Never shell-interpreted
	// unless the profile itself names a shell (see Custom).
	Command string
	Args    []string

	// urlPattern matches the verification URL in the child's output. Kept
	// per-profile rather than one global regex: a flow that prints several URLs
	// (docs links, error references) needs a pattern tight enough to pick the
	// right one.
	urlPattern *regexp.Regexp
}

// Spec renders the profile as the process spec proc.Start expects.
func (p Profile) Spec() proc.Spec {
	return proc.Spec{Command: p.Command, Args: p.Args}
}

// ExtractURL returns the verification URL found in a line of child output.
// Trailing punctuation is trimmed: CLIs commonly wrap the URL in quotes or end
// the sentence with a period, neither of which belongs in the link.
func (p Profile) ExtractURL(line string) (string, bool) {
	if p.urlPattern == nil {
		return "", false
	}
	match := p.urlPattern.FindString(line)
	if match == "" {
		return "", false
	}
	return strings.TrimRight(match, `.,;:'")]>`), true
}

// Succeeded reports whether the finished run authenticated successfully. A nil
// error from proc.Handle.Wait means a zero exit status, which every supported
// flow uses to signal success.
func Succeeded(waitErr error) bool { return waitErr == nil }

// googleAuthURL matches the consent URL gcloud prints. It is anchored on
// Google's own host so a docs link in the same output cannot be mistaken for
// the thing the user is supposed to open.
var googleAuthURL = regexp.MustCompile(`https://accounts\.google\.com/\S+`)

// anyHTTPSURL is the fallback for custom flows, whose output shape is unknown
// to us. Deliberately https-only: an auth handshake sent over plaintext is not
// something to offer the user as a button.
var anyHTTPSURL = regexp.MustCompile(`https://\S+`)

// claudeAuthURL matches the consent URL `claude auth login` prints. It is
// anchored on the oauth/authorize path, not merely the host: the same output
// carries a redirect_uri pointing at platform.claude.com, and a looser pattern
// would happily hand the admin the callback URL instead of the consent page.
var claudeAuthURL = regexp.MustCompile(`https://claude\.com/\S*oauth/authorize\S+`)

// builtins are the profiles that ship with the tool, keyed by the name the
// agent passes. `aws` is intentionally absent until its flow is implemented —
// a profile that posts a card and then cannot complete would be worse than a
// clear "unknown profile" error.
var builtins = map[string]Profile{
	"gcloud": {
		Name:       "gcloud",
		NeedsCode:  true,
		Command:    "gcloud",
		Args:       []string{"auth", "login", "--no-launch-browser"},
		urlPattern: googleAuthURL,
	},
	"gcloud-adc": {
		Name:       "gcloud-adc",
		NeedsCode:  true,
		Command:    "gcloud",
		Args:       []string{"auth", "application-default", "login", "--no-launch-browser"},
		urlPattern: googleAuthURL,
	},
	// claude-code re-authenticates the Claude Code CLI itself — the credential
	// every `claude_code` agent runs on.
	//
	// It drives `claude auth login`, NOT `claude setup-token`. Both share the
	// same URL-and-paste-back component, so either would fit this card, but
	// setup-token mints a token valid for about a year and scoped to inference
	// only. That reshapes the expiry problem into a slower one and quietly drops
	// capability; `auth login` produces the ordinary rotating credential in the
	// ordinary store, which is what the rest of the system already expects.
	//
	// `--claudeai` is required, not cosmetic: without it the CLI opens with an
	// interactive "select login method" menu that a headless pipe cannot
	// navigate, and the flow parks there forever instead of printing a URL.
	//
	// Verified against CLI 2.1.238: with stdin and stdout as plain pipes the
	// process prints the consent URL, then prompts `Paste code here if prompted >`
	// WITHOUT a trailing newline (proc surfaces that as a partial line) and reads
	// the answer straight off stdin. No pty is needed.
	"claude-code": {
		Name:       "claude-code",
		NeedsCode:  true,
		Command:    "claude",
		Args:       []string{"auth", "login", "--claudeai"},
		urlPattern: claudeAuthURL,
	},
}

// CustomProfileName is the profile that runs a caller-supplied command.
const CustomProfileName = "custom"

// Lookup returns the named built-in profile. `custom` is not returned here: it
// carries no command of its own and must be built with Custom.
func Lookup(name string) (Profile, bool) {
	p, ok := builtins[strings.TrimSpace(name)]
	return p, ok
}

// Names lists the selectable profile names, including custom, in a stable
// order for help text and error messages.
func Names() []string {
	names := make([]string, 0, len(builtins)+1)
	for name := range builtins {
		names = append(names, name)
	}
	names = append(names, CustomProfileName)
	sort.Strings(names)
	return names
}

// Custom builds a profile from a caller-supplied command line.
//
// The command runs through `sh -c`, because callers describe these as command
// *lines* — with pipes, redirects and quoting — rather than as an argv. That is
// an explicit shell-execution primitive: whatever the agent passes here is
// executed. It is gated by the fact that an admin has to approve every auth
// request before anything runs, which is the control that makes it acceptable;
// it is not safe on its own.
//
// needsCode selects the card layout, exactly as for a built-in.
func Custom(command string, needsCode bool) (Profile, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return Profile{}, fmt.Errorf("auth: the %q profile requires a command", CustomProfileName)
	}
	return Profile{
		Name:       CustomProfileName,
		NeedsCode:  needsCode,
		Command:    "sh",
		Args:       []string{"-c", command},
		urlPattern: anyHTTPSURL,
	}, nil
}

// Resolve picks the profile for a tool invocation. command and needsCode are
// only consulted for the custom profile; passing a command alongside a built-in
// is an error rather than a silently ignored argument.
func Resolve(name, command string, needsCode bool) (Profile, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Profile{}, fmt.Errorf("auth: a profile is required (one of: %s)", strings.Join(Names(), ", "))
	}
	if name == CustomProfileName {
		return Custom(command, needsCode)
	}
	p, ok := Lookup(name)
	if !ok {
		return Profile{}, fmt.Errorf("auth: unknown profile %q (expected one of: %s)", name, strings.Join(Names(), ", "))
	}
	if strings.TrimSpace(command) != "" {
		return Profile{}, fmt.Errorf("auth: profile %q runs a fixed command; `command` is only valid with the %q profile", name, CustomProfileName)
	}
	return p, nil
}
