package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/journal"
)

// Turn outcomes recorded on the acp_session stream.
const (
	turnCompleted   = "completed"
	turnErrored     = "errored"
	turnTimedOut    = "timed_out"
	turnInterrupted = "interrupted"
)

// sessionLogger persists ACP chat turns to the journal's acp_session stream:
// the queryable envelope (agent, channel/user, outcome, metrics) as a row, and
// the full prompt/response text as a per-session transcript blob the row points
// at. It is wired only when the acp_session stream is enabled, so a nil logger
// (the default) records nothing.
type sessionLogger struct {
	recorder journal.Recorder
	blobs    *journal.BlobStore
	logger   *slog.Logger
}

// newSessionLogger builds a logger writing rows through recorder and transcripts
// under blobDir.
func newSessionLogger(recorder journal.Recorder, blobDir string, logger *slog.Logger) *sessionLogger {
	if logger == nil {
		logger = slog.Default()
	}
	return &sessionLogger{recorder: recorder, blobs: journal.NewBlobStore(blobDir), logger: logger}
}

// sessionTurn is one completed ACP chat turn to record.
type sessionTurn struct {
	req        ChatRequest
	agent      string
	sessionID  string
	prompt     string
	response   string
	outcome    string
	stopReason string
	duration   time.Duration
	chunks     int
	bytes      int
	// errText is the terminal error message for an errored turn (empty otherwise),
	// so a failed turn is explainable from the journal row alone.
	errText string
}

// truncateForSummary bounds an error string embedded in a one-line summary, so a
// long cause does not bloat the queryable index (the full text stays in the
// payload's "error" field).
func truncateForSummary(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// record writes the transcript blob (best-effort) and the journal row. A blob
// write failure logs and the row is still recorded, just without a blob_ref, so
// the queryable index never depends on the filesystem succeeding.
func (s *sessionLogger) record(ctx context.Context, t sessionTurn) {
	if s == nil {
		return
	}
	ref, err := s.blobs.AppendTranscript(t.sessionID, journal.TranscriptTurn{
		Time:     time.Now(),
		Agent:    t.agent,
		Source:   t.req.Source,
		Outcome:  t.outcome,
		Prompt:   t.prompt,
		Response: t.response,
	})
	if err != nil {
		s.logger.Warn("failed to write session transcript", "session_id", t.sessionID, "error", err)
		ref = ""
	}

	level := journal.LevelInfo
	switch t.outcome {
	case turnErrored:
		level = journal.LevelError
	case turnTimedOut:
		level = journal.LevelWarn
	}

	summary := fmt.Sprintf("%s turn via %s (%d bytes)", t.outcome, t.agent, t.bytes)
	if t.errText != "" {
		summary += ": " + truncateForSummary(t.errText, 160)
	}

	payload := map[string]any{
		"agent":       t.agent,
		"source":      t.req.Source,
		"outcome":     t.outcome,
		"stop_reason": t.stopReason,
		"duration_ms": t.duration.Milliseconds(),
		"chunks":      t.chunks,
		"bytes":       t.bytes,
	}
	if t.errText != "" {
		payload["error"] = t.errText
	}

	s.recorder.Record(ctx, journal.Event{
		Stream:  journal.StreamACPSession,
		Kind:    "session.turn",
		Level:   level,
		Summary: summary,
		Keys: journal.Keys{
			TeamID:    t.req.TeamID,
			ChannelID: t.req.ChannelID,
			ThreadTS:  t.req.ThreadTS,
			UserID:    t.req.UserID,
			SessionID: t.sessionID,
		},
		BlobRef: ref,
		Payload: payload,
	})
}

// recordEviction writes one dropped session to the acp_session stream.
//
// An idle sweep is expected and is never shown to the user, which is precisely
// why it belongs here: without a row, the only trace a conversation was dropped
// is the absence of one, and "why did the agent forget my thread?" becomes
// unanswerable after the fact. A busy-timeout eviction is the one that
// interrupted work, so it is the one recorded as a warning.
//
// Called from the session manager's observer, which runs under its lock: the
// recorder's Record is a non-blocking enqueue, so this must stay allocation-
// light and must never grow a blocking call.
func (s *sessionLogger) recordEviction(ctx context.Context, agentName string, e agent.Eviction) {
	if s == nil {
		return
	}
	level := journal.LevelInfo
	if e.Reason == agent.EvictionBusyTimeout {
		level = journal.LevelWarn
	}
	s.recorder.Record(ctx, journal.Event{
		Stream:  journal.StreamACPSession,
		Kind:    "session.evicted",
		Level:   level,
		Summary: fmt.Sprintf("evicted %s session (%s) after %s", agentName, e.Reason, e.Age.Round(time.Second)),
		Keys: journal.Keys{
			TeamID:    e.Key.TeamID,
			ChannelID: e.Key.ChannelID,
			ThreadTS:  e.Key.ThreadTS,
			SessionID: e.SessionID,
		},
		Payload: map[string]any{
			"agent":   agentName,
			"reason":  string(e.Reason),
			"age_ms":  e.Age.Milliseconds(),
			"dm":      e.Key.DM,
			"live":    e.Live,
			"mid_run": e.Reason == agent.EvictionBusyTimeout,
		},
	})
}
