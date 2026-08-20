// Package replyblock renders an agent's reply text as Block Kit JSON for the
// non-streaming (buffered) transport.
//
// # Why this exists
//
// Murtaugh has two ways of putting reply text into Slack, and they parse
// different dialects. The streaming transport sends a `markdown_text` chunk
// (chat.appendStream), which takes standard Markdown — `**bold**`, tables,
// task lists — and resolves `<@U…>` mentions itself. The buffered transport
// posts with chat.postMessage, whose `text` field is Slack `mrkdwn` —
// `*bold*`, `<url|text>`. The same agent prose therefore rendered correctly on
// one path and showed raw metacharacters on the other, depending only on which
// transport the surface happened to use.
//
// Wrapping the buffered reply in a `markdown` block settles that: both paths
// now take standard Markdown, so the agent has one dialect to write and the
// prompt has one rule to state.
//
// # The mention trade-off
//
// A `markdown` block does NOT resolve `<@U…>` into a real mention — it renders
// the raw reference as literal text and nobody gets notified. So Rewrite pulls
// every Slack reference out of the prose, replaces it with readable plain text
// (`@Miere`), and returns the references separately; the template re-emits them
// as a trailing `rich_text` block, which does notify.
//
// The ping therefore still fires, but it is relocated: "I asked <@U1> to review
// and <@U2> to deploy" renders a footer of "@ann @bob 👆" with no indication of
// who does what. The inline text preserves the meaning; the footer exists purely
// as a notification carrier.
//
// # Rendering rule
//
// Blocks are built by rendering a JSON template, never with slack-go's typed
// builders — see the "Block Kit rendering" section of ARCHITECTURE.md. The
// bytes reach Slack untouched via client.DecodeBlocks' rawBlock passthrough.
package replyblock

import (
	"fmt"
	"io/fs"
	"regexp"
	"strings"

	"github.com/miere/murtaugh/assets"
	"github.com/miere/murtaugh/internal/jsontemplate"
)

// Template is the reply document, resolved against the config dir first and the
// embedded assets tree second — so an operator can restyle it without a rebuild.
const Template = "templates/reply/markdown.json"

// Mention kinds. These name the rich_text element each reference becomes; the
// template branches on them.
const (
	KindUser      = "user"      // <@U123>      → {"type":"user","user_id":…}
	KindBroadcast = "broadcast" // <!here>      → {"type":"broadcast","range":…}
	KindUserGroup = "usergroup" // <!subteam^S1>→ {"type":"usergroup","usergroup_id":…}
)

// Mention is one notification target lifted out of the reply prose.
type Mention struct {
	Kind string
	ID   string
}

// slackRefPattern matches Slack's mrkdwn reference syntax: a sigil, an id, and
// an optional "|label" fallback. It deliberately does not match `<https://…>`
// links, whose sigil-less body is left for the Markdown renderer to handle.
var slackRefPattern = regexp.MustCompile(`<([@#!])([^>|]*)(?:\|([^>]*))?>`)

// Rewrite replaces every Slack reference in text with plain readable text and
// returns the mentions that need re-emitting as rich_text.
//
// name resolves a user id to a display name; it may be nil, and may return ""
// for an id it cannot resolve (deactivated account, external user, API
// hiccup). Either way the rewrite fails soft — the raw id renders as "@U123"
// and the mention is STILL collected, so the notification lands even when the
// name does not. Dropping it instead would silently lose a ping.
//
// Mentions are deduplicated and returned in first-appearance order: the same
// person tagged three times is one footer entry.
//
// Channel references (`<#C123|general>`) flatten to "#general" with no mention
// entry — there is nothing to notify, so there is nothing to relocate.
func Rewrite(text string, name func(id string) string) (string, []Mention) {
	var mentions []Mention
	seen := make(map[string]bool)
	collect := func(kind, id string) {
		key := kind + ":" + id
		if id == "" || seen[key] {
			return
		}
		seen[key] = true
		mentions = append(mentions, Mention{Kind: kind, ID: id})
	}

	rewritten := slackRefPattern.ReplaceAllStringFunc(text, func(match string) string {
		groups := slackRefPattern.FindStringSubmatch(match)
		sigil, body, label := groups[1], groups[2], groups[3]
		switch sigil {
		case "@":
			collect(KindUser, body)
			return "@" + userLabel(body, label, name)
		case "#":
			return "#" + first(label, body)
		case "!":
			return rewriteBang(body, label, collect)
		}
		return match
	})
	return rewritten, mentions
}

// rewriteBang handles the `<!…>` family: the two broadcasts and user groups.
// An unrecognised `<!…>` is left exactly as written rather than guessed at —
// Slack may grow new ones, and passing an unknown reference through unchanged
// is the only harmless option.
func rewriteBang(body, label string, collect func(kind, id string)) string {
	if group, ok := strings.CutPrefix(body, "subteam^"); ok {
		collect(KindUserGroup, group)
		return "@" + strings.TrimPrefix(first(label, group), "@")
	}
	switch body {
	case "here", "channel", "everyone":
		collect(KindBroadcast, body)
		return "@" + body
	}
	return "<!" + body + ">"
}

// userLabel picks the text shown in place of a user reference. The resolver
// wins because it reports the current display name; the agent-supplied "|label"
// is a stale legacy form, and the bare id is the last resort.
func userLabel(id, label string, name func(string) string) string {
	if name != nil {
		if resolved := strings.TrimSpace(name(id)); resolved != "" {
			return resolved
		}
	}
	return first(label, id)
}

func first(preferred, fallback string) string {
	if preferred != "" {
		return preferred
	}
	return fallback
}

// Renderer turns reply text into the raw Block Kit JSON the Slack client posts
// verbatim. The template lookup, escaping funcs and missingkey=error discipline
// live in internal/jsontemplate; this type only supplies the data.
type Renderer struct {
	tpl *jsontemplate.Renderer
}

// NewRenderer builds a Renderer. A nil templateFS falls back to assets.FS, so
// the shipped template resolves out of the box.
func NewRenderer(templateDir string, templateFS fs.FS) *Renderer {
	if templateFS == nil {
		templateFS = assets.FS
	}
	return &Renderer{tpl: jsontemplate.New(templateDir, templateFS)}
}

// Render returns the blocks document for text plus its mentions. The text is
// interpolated with `json`, not pasted in: it is agent prose and will contain
// quotes, newlines and backslashes, none of which text/template escapes.
func (r *Renderer) Render(text string, mentions []Mention) ([]byte, error) {
	out, err := r.tpl.Render(Template, struct {
		Text     string
		Mentions []Mention
	}{Text: text, Mentions: mentions})
	if err != nil {
		return nil, fmt.Errorf("replyblock: render %s: %w", Template, err)
	}
	return out, nil
}
