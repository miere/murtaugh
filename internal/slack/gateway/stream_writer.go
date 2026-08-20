package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/slack-go/slack"
)

// maxStreamMessageChars is Slack's hard limit on one streaming message. It counts
// *characters* and applies to the message *total*, not to each append — three
// 4000-char appends fill it exactly as one 12000-char append does.
//
// Both facts were measured against the real API (ignore/streamprobe), because
// getting either wrong changes the fix: 12000 was accepted and 12001 returned
// msg_too_long; 4000+4000+4000 was accepted and a fourth append on the same
// message was refused; 6000 em-dashes (18000 bytes) was accepted, so the unit is
// runes, not bytes.
const maxStreamMessageChars = 12000

// streamMessageBudget is what we actually spend of maxStreamMessageChars, holding
// back enough room to close an open code fence on the way out of a message.
const streamMessageBudget = maxStreamMessageChars - 64

// streamMinRoom is the smallest tail of a message worth filling. Below it we roll
// over early rather than cram a few words in, which would break a paragraph
// across two messages to save space nobody wanted.
const streamMinRoom = 512

type StreamWriter struct {
	api       StreamAPI
	channelID string
	threadTS  string
	teamID    string
	userID    string
	interval  time.Duration
	minChars  int
	logger    *slog.Logger

	streamChannel string
	streamTS      string
	pending       string
	lastFlush     time.Time
	flushes       int
	bytesFlushed  int
	started       bool
	stopped       bool
	// spent is how much of streamMessageBudget the *current* message has used, in
	// runes. Reset on rollover; distinct from bytesFlushed, which stays cumulative
	// across rollovers because it reports the size of the whole reply.
	spent int
	// fence follows code-fence state so a rollover can close and reopen a code
	// block that spans the boundary.
	fence fenceTracker
}

func NewStreamWriter(api StreamAPI, channelID string, opts StreamWriterOptions) *StreamWriter {
	if opts.Interval <= 0 {
		opts.Interval = 250 * time.Millisecond
	}
	if opts.MinChars <= 0 {
		opts.MinChars = 24
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &StreamWriter{api: api, channelID: channelID, threadTS: opts.ThreadTS, teamID: opts.TeamID, userID: opts.UserID, interval: opts.Interval, minChars: opts.MinChars, logger: logger}
}

type StreamWriterOptions struct {
	ThreadTS string
	TeamID   string
	UserID   string
	Interval time.Duration
	MinChars int
	Logger   *slog.Logger
	// TemplateDir and ResolveUserName serve the buffered transport only —
	// StreamWriter ignores both. They live here because the two sinks are built
	// from one options value (see newDefaultSlackSink), and splitting the bag in
	// two would push the same choice onto every construction site.
	//
	// TemplateDir is where the reply block template is looked up before falling
	// back to the embedded assets tree; empty means the working directory.
	TemplateDir string
	// ResolveUserName maps a Slack user id to a display name for the buffered
	// reply's mention rewrite. nil (and "" results) fail soft — the raw id shows
	// instead of a name, and the mention is still emitted.
	ResolveUserName func(ctx context.Context, userID string) string
}

func (w *StreamWriter) Start(ctx context.Context) error {
	return w.StartWithOptions(ctx)
}

func (w *StreamWriter) StartWithOptions(ctx context.Context, extraOptions ...slack.MsgOption) error {
	if w.started {
		return nil
	}
	// Plan mode groups every task_update chunk under a single Plan block
	// (with a shared title) instead of stacking them as a flat timeline of
	// separate cards. TaskCardWriter opens the plan with a plan_update chunk
	// on the first task. See https://docs.slack.dev/reference/block-kit/blocks/plan-block/
	options := []slack.MsgOption{slack.MsgOptionTaskDisplayMode(slack.TaskDisplayModePlan)}
	options = append(options, extraOptions...)
	if w.threadTS != "" {
		options = append(options, slack.MsgOptionTS(w.threadTS))
	}
	if w.teamID != "" {
		options = append(options, slack.MsgOptionRecipientTeamID(w.teamID))
	}
	if w.userID != "" {
		options = append(options, slack.MsgOptionRecipientUserID(w.userID))
	}
	channel, ts, err := w.api.StartStreamContext(ctx, w.channelID, options...)
	if err != nil {
		return fmt.Errorf("start Slack stream: %w", err)
	}
	w.streamChannel = channel
	w.streamTS = ts
	w.lastFlush = time.Now()
	w.started = true
	return nil
}

func (w *StreamWriter) StreamChannel() string { return w.streamChannel }
func (w *StreamWriter) StreamTS() string      { return w.streamTS }
func (w *StreamWriter) Started() bool         { return w.started }
func (w *StreamWriter) Stopped() bool         { return w.stopped }

// Append buffers a reply delta and paints it on a *coherent boundary*, never
// mid-thought. Coherence here is a paint concern only — the segmenter has
// already sealed this run against any tool activity, so buffering can never
// reorder content, only pace how a text run grows on screen:
//
//   - the first delta paints eagerly, so the reply appears promptly;
//   - thereafter we flush through the last newline, so complete lines (lists,
//     code, paragraphs) land whole;
//   - a long or stale unbroken line still streams (the latency/size cap), but is
//     trimmed to the last space so prose never repaints mid-word.
//
// Slack's streaming API concatenates appends into one growing message, so a
// retained trailing fragment is simply prepended to the next flush — splitting
// on a boundary changes *when* the paint happens, never the final text.
func (w *StreamWriter) Append(ctx context.Context, text string) error {
	if text == "" || w.stopped {
		return nil
	}
	if err := w.Start(ctx); err != nil {
		return err
	}
	w.pending += text
	if w.flushes == 0 {
		return w.Flush(ctx)
	}
	if i := strings.LastIndexByte(w.pending, '\n'); i >= 0 {
		return w.emit(ctx, i+1)
	}
	if len(w.pending) >= w.minChars || time.Since(w.lastFlush) >= w.interval {
		if i := strings.LastIndexByte(w.pending, ' '); i >= 0 {
			return w.emit(ctx, i+1)
		}
		return w.Flush(ctx)
	}
	return nil
}

// Flush paints all buffered text. Used on seal (Stop) and interjection.
func (w *StreamWriter) Flush(ctx context.Context) error {
	return w.emit(ctx, len(w.pending))
}

// emit paints the first n bytes of the buffer to Slack and retains the rest for
// the next flush. n == 0 (or an unstarted/stopped stream) is a no-op. The buffer
// is consumed only once the paint lands, so a failed append never drops text.
func (w *StreamWriter) emit(ctx context.Context, n int) error {
	if !w.started || w.stopped || n == 0 {
		return nil
	}
	text := w.pending[:n]
	startedAt := time.Now()
	if err := w.paint(ctx, text); err != nil {
		return err
	}
	w.pending = w.pending[n:]
	w.flushes++
	w.bytesFlushed += len(text)
	w.logger.Info("appended Slack stream chunk", "channel", w.streamChannel, "bytes", len(text), "duration", time.Since(startedAt), "flushes", w.flushes)
	w.lastFlush = time.Now()
	return nil
}

// paint puts text on the wire, spanning as many streaming messages as it takes.
//
// A streaming message holds maxStreamMessageChars in *total*, so a long reply
// cannot be delivered by one message however finely we chop the appends — it has
// to continue in a new one. paint therefore owns two decisions the caller should
// not have to think about: where to cut (the widest boundary that fits, via
// splitAtBudget) and when to roll over (before the cut, so no append is ever sent
// that we expect Slack to refuse).
//
// This is the pre-emptive half of the size story. append keeps a reactive half
// for the case where our accounting and Slack's disagree.
func (w *StreamWriter) paint(ctx context.Context, text string) error {
	for text != "" {
		// Roll over early on a nearly-full message rather than wedge a fragment
		// into the tail. Guarded on spent > 0 so a fresh message always takes
		// text, which is what keeps this loop finite.
		if w.spent > 0 && streamMessageBudget-w.spent < streamMinRoom {
			if err := w.rollover(ctx); err != nil {
				return err
			}
		}
		piece, rest := splitAtBudget(text, streamMessageBudget-w.spent)
		if err := w.append(ctx, piece); err != nil {
			return err
		}
		w.spent += utf8.RuneCountInString(piece)
		w.fence.consume(piece)
		text = rest
	}
	return nil
}

// splitAtBudget cuts text so the first piece fits in room characters, preferring
// the widest boundary that fits: a blank line (paragraph), then a line end, then a
// word break. A run with none of those — a minified file, a base64 payload — is
// cut at the budget on a rune boundary. Ugly, but it moves, which beats the reply
// vanishing.
//
// Byte indexes taken from window are valid in text because window is a prefix of
// it; only the final hard cut needs the rune slice, to avoid splitting a
// multi-byte character in half.
func splitAtBudget(text string, room int) (piece, rest string) {
	if room <= 0 {
		return "", text
	}
	runes := []rune(text)
	if len(runes) <= room {
		return text, ""
	}
	window := string(runes[:room])
	if i := strings.LastIndex(window, "\n\n"); i > 0 {
		return text[:i+2], text[i+2:]
	}
	if i := strings.LastIndexByte(window, '\n'); i > 0 {
		return text[:i+1], text[i+1:]
	}
	if i := strings.LastIndexByte(window, ' '); i > 0 {
		return text[:i+1], text[i+1:]
	}
	return window, string(runes[room:])
}

// append paints text onto the live streaming message, transparently rolling over
// to a fresh message when Slack refuses it for a reason a new message would cure.
// There are two:
//
//   - message_not_in_streaming_state — the message got too *old*. Slack caps how
//     long a streaming message stays open, and a long turn (a coding agent
//     reading, planning, and writing for several minutes) routinely outlives it.
//   - msg_too_long — the message got too *full*. paint's budget should mean we
//     never send one of these; this is the net for when Slack moves the number or
//     counts something we do not, and it is deliberately not fatal.
//
// Either way, failing the turn would leave the user with a half-sentence and no
// error, so we continue in a new message instead. The text carries across, so the
// reply simply spans two messages rather than being lost.
func (w *StreamWriter) append(ctx context.Context, text string) error {
	_, _, err := w.api.AppendStreamContext(ctx, w.streamChannel, w.streamTS, slack.MsgOptionChunks(slack.NewMarkdownTextChunk(text)))
	if err == nil {
		return nil
	}
	if !isStreamFinalized(err) && !isMessageTooLong(err) {
		return fmt.Errorf("append Slack stream: %w", err)
	}
	if isMessageTooLong(err) {
		// Worth a warning rather than silence: it means streamMessageBudget no
		// longer matches reality and the constant needs re-measuring.
		w.logger.Warn("Slack refused a stream append as too long despite the size budget; rolling over",
			"chars", utf8.RuneCountInString(text), "spent", w.spent, "budget", streamMessageBudget)
	}
	if rerr := w.rollover(ctx); rerr != nil {
		return rerr
	}
	if _, _, err := w.api.AppendStreamContext(ctx, w.streamChannel, w.streamTS, slack.MsgOptionChunks(slack.NewMarkdownTextChunk(text))); err != nil {
		return fmt.Errorf("append Slack stream: %w", err)
	}
	return nil
}

// rollover leaves the current streaming message and opens a fresh one in its
// place, so a turn that outgrows one message — in age or in size — keeps rendering
// rather than dying. Callers are responsible for re-emitting whatever content the
// old message rejected (see append and TaskCardWriter.Update).
//
// Two pieces of housekeeping on the way out, both best-effort because the message
// we are leaving may already be finalized, and neither is worth failing a turn the
// agent has finished:
//
//   - close an open code fence, and reopen it on the new message. Without this a
//     code block spanning the boundary renders as code on one half and raw
//     backticked prose on the other. This used to be a rarity that only long-lived
//     turns hit; with a size budget it is on the main path for any long reply.
//   - stop the old message, so it does not sit in streaming state (still showing
//     as generating) until Slack's window elapses on its own.
func (w *StreamWriter) rollover(ctx context.Context) error {
	prev := w.streamTS
	reopen := w.fence.reopen()
	if closer := w.fence.closer(); closer != "" {
		if _, _, err := w.api.AppendStreamContext(ctx, w.streamChannel, w.streamTS, slack.MsgOptionChunks(slack.NewMarkdownTextChunk(closer))); err != nil {
			w.logger.Debug("could not close the code fence before rollover", "error", err)
		}
	}
	if _, _, err := w.api.StopStreamContext(ctx, w.streamChannel, w.streamTS); err != nil && !isStreamFinalized(err) {
		w.logger.Debug("could not stop the streaming message before rollover", "error", err)
	}
	w.started = false
	w.stopped = false
	w.streamChannel = ""
	w.streamTS = ""
	w.spent = 0
	w.fence.rolledOver()
	if err := w.Start(ctx); err != nil {
		return err
	}
	w.logger.Info("rolled over Slack stream", "prev_ts", prev, "new_ts", w.streamTS, "in_fence", reopen != "")
	if reopen != "" {
		if _, _, err := w.api.AppendStreamContext(ctx, w.streamChannel, w.streamTS, slack.MsgOptionChunks(slack.NewMarkdownTextChunk(reopen))); err != nil {
			return fmt.Errorf("reopen code fence after rollover: %w", err)
		}
		w.spent += utf8.RuneCountInString(reopen)
	}
	return nil
}

// fenceTracker follows fenced-code-block state across arbitrary chunk boundaries,
// so rollover can tell whether the message it is leaving ends mid-code-block.
// Chunks do not arrive line-aligned, hence the partial-line carry.
type fenceTracker struct {
	partial string // text since the last newline
	opener  string // the opening delimiter line ("```go"); empty when outside a fence
}

func (f *fenceTracker) consume(text string) {
	f.partial += text
	for {
		i := strings.IndexByte(f.partial, '\n')
		if i < 0 {
			return
		}
		line := strings.TrimSpace(f.partial[:i])
		f.partial = f.partial[i+1:]
		switch {
		case f.opener == "" && isFenceDelimiter(line):
			f.opener = line
		case f.opener != "" && strings.HasPrefix(line, f.opener[:3]):
			f.opener = ""
		}
	}
}

// closer is what to append to the message being left so its code block terminates.
// The leading newline covers a chunk that ended mid-line.
func (f *fenceTracker) closer() string {
	if f.opener == "" {
		return ""
	}
	return "\n" + f.opener[:3] + "\n"
}

// reopen is what to prepend to the new message so the code block continues, with
// the same info string (and so the same syntax highlighting) as the original.
func (f *fenceTracker) reopen() string {
	if f.opener == "" {
		return ""
	}
	return f.opener + "\n"
}

// rolledOver notes that closer()/reopen() have been painted: the carried partial
// line was terminated by the closer, but we are still logically inside the fence,
// so opener stands.
func (f *fenceTracker) rolledOver() { f.partial = "" }

func isFenceDelimiter(line string) bool {
	return strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~")
}

// Fail paints the failure notice and seals the message. The notice is the user's
// only signal that a turn died, so it must land even when the buffered text
// cannot: we try to paint what is pending, then drop it regardless and paint the
// notice on a clean buffer.
//
// Dropping is the whole point. emit deliberately retains pending on error so a
// transient failure never loses text — but that retention is poison here, because
// the pending text is often *why* we are failing. Painting the notice on top of it
// re-sent the same rejected bytes, failed identically, and returned before Stop —
// so the reply, the notice and the error all disappeared, leaving an empty
// streaming message and one line in stderr. That is how msg_too_long stayed
// invisible for three days.
func (w *StreamWriter) Fail(ctx context.Context, err error) error {
	if flushErr := w.Flush(ctx); flushErr != nil {
		w.logger.Warn("dropping unpainted reply text so the failure notice can land",
			"chars", utf8.RuneCountInString(w.pending), "error", flushErr)
	}
	w.pending = ""
	if appendErr := w.Append(ctx, streamFailMessage(err)); appendErr != nil {
		return appendErr
	}
	return w.Stop(ctx)
}

func (w *StreamWriter) Stop(ctx context.Context) error {
	if err := w.Flush(ctx); err != nil {
		return err
	}
	if !w.started || w.stopped {
		return nil
	}
	startedAt := time.Now()
	_, _, err := w.api.StopStreamContext(ctx, w.streamChannel, w.streamTS)
	if err != nil {
		if isStreamFinalized(err) {
			// Slack already closed the message (its streaming window elapsed while
			// the turn ran on). There is nothing left to stop, so this is a clean
			// finish, not a failure — surfacing it would abort the turn on teardown.
			w.stopped = true
			return nil
		}
		return fmt.Errorf("stop Slack stream: %w", err)
	}
	w.stopped = true
	w.logger.Info("stopped Slack stream", "channel", w.streamChannel, "flushes", w.flushes, "bytes", w.bytesFlushed, "duration", time.Since(startedAt))
	return nil
}

// streamFinalizedError is the Slack API error returned when we append to or stop
// a streaming message it has already closed — typically because the message hit
// Slack's maximum streaming lifetime while a long turn was still in flight.
const streamFinalizedError = "message_not_in_streaming_state"

// streamTooLongError is the Slack API error returned when an append would push a
// streaming message past maxStreamMessageChars.
const streamTooLongError = "msg_too_long"

// isStreamFinalized reports whether err is Slack rejecting a stream operation
// because the message is no longer in streaming state. This is an expected,
// recoverable condition on long turns (roll over to a fresh message), never a
// reason to fail the turn.
func isStreamFinalized(err error) bool {
	return err != nil && strings.Contains(err.Error(), streamFinalizedError)
}

// isMessageTooLong reports whether err is Slack refusing an append because the
// message is full. Like isStreamFinalized, it is cured by a fresh message, not by
// failing the turn.
func isMessageTooLong(err error) bool {
	return err != nil && strings.Contains(err.Error(), streamTooLongError)
}

func sanitizeSlackInline(text string) string {
	text = strings.ReplaceAll(text, "`", "'")
	if len(text) > 300 {
		return text[:300] + "…"
	}
	return text
}
