package gateway

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/journal"
	"github.com/miere/murtaugh/internal/slack/alertcard"
	slackclient "github.com/miere/murtaugh/internal/slack/client"
	"github.com/miere/murtaugh/internal/slack/replyblock"
)

const inlineReplyBytes = 2 << 10

func jobReplyBlobs(cfg config.Config) *journal.BlobStore {
	if !cfg.Journal.EffectiveEnabled(journal.StreamJob) {
		return nil
	}
	return journal.NewBlobStore(cfg.Journal.EffectiveBlobDir(cfg.BaseDir, cfg.BaseName))
}

func (a *Gateway) settleJobReply(ctx context.Context, name string, reply *agentruntime.Reply) {
	dest := strings.TrimSpace(a.scheduledJobs[name].ReportTo)
	payload := map[string]any{}
	if dest != "" {
		payload["report_to"] = dest
	}
	if reply == nil {
		if dest != "" {
			a.recordReply(ctx, journal.LevelWarn, name, "", "", payload, "missing",
				fmt.Sprintf("report for job %q withheld: the run handed no reply back", name))
		}
		return
	}
	payload["node_id"], payload["node_owner"], payload["in_process"] = reply.NodeID, reply.NodeOwner, reply.InProcess
	if why := a.withholdReason(*reply); why != "" {
		a.recordReply(ctx, journal.LevelWarn, name, "", "", payload, "withheld",
			fmt.Sprintf("reply for job %q withheld: %s", name, why))
		if dest != "" {
			a.notifyWithheldReport(ctx, name, *reply, why)
		}
		return
	}
	a.markReportAllowed(name)
	text := strings.TrimSpace(reply.Text)
	blobRef := a.keepReply(payload, name, text)
	switch {
	case dest == "":
		a.recordReply(ctx, journal.LevelInfo, name, "", blobRef, payload, "kept",
			fmt.Sprintf("reply for job %q kept; the job reports nowhere", name))
	case text == "":
		a.recordReply(ctx, journal.LevelWarn, name, "", blobRef, payload, "empty",
			fmt.Sprintf("report for job %q not posted: the agent's reply was empty", name))
	default:
		channel, err := a.postReport(ctx, dest, text)
		if err != nil {
			payload["error"] = err.Error()
			a.logger.Error("could not report a scheduled job", "job", name, "report_to", dest, "error", err)
			a.recordReply(ctx, journal.LevelError, name, channel, blobRef, payload, "failed",
				fmt.Sprintf("report for job %q could not be posted to %s: %s", name, dest, truncateForSummary(err.Error(), 160)))
			return
		}
		a.recordReply(ctx, journal.LevelInfo, name, channel, blobRef, payload, "delivered",
			fmt.Sprintf("report for job %q posted to %s", name, dest))
	}
}

func (a *Gateway) withholdReason(reply agentruntime.Reply) string {
	if reply.InProcess {
		return ""
	}
	access := a.access()
	if access.IsAdminUser(reply.NodeOwner) {
		return ""
	}
	if strings.TrimSpace(access.AdminUser) == "" {
		return fmt.Sprintf("node %q ran it and this gateway has no admin to own it", reply.NodeID)
	}
	return fmt.Sprintf("node %q belongs to %q, not the gateway admin", reply.NodeID, reply.NodeOwner)
}

func (a *Gateway) keepReply(payload map[string]any, name, text string) string {
	if len(text) <= inlineReplyBytes {
		payload["reply"] = text
		return ""
	}
	cut := inlineReplyBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	payload["reply"] = text[:cut] + "…"
	payload["reply_bytes"] = len(text)
	ref, err := a.replyBlobs.AppendTranscript(fmt.Sprintf("job-%s-%d", name, time.Now().UnixNano()), journal.TranscriptTurn{
		Time:     time.Now(),
		Source:   "job",
		Outcome:  "completed",
		Prompt:   a.scheduledJobs[name].Prompt,
		Response: text,
	})
	if err != nil {
		payload["reply_blob_error"] = err.Error()
		return ""
	}
	return ref
}

func (a *Gateway) notifyWithheldReport(ctx context.Context, name string, reply agentruntime.Reply, why string) {
	if a.alreadyWithheld(name) {
		return
	}
	admin := strings.TrimSpace(a.access().AdminUser)
	if admin == "" {
		return
	}
	spec := alertcard.Spec{
		Level:     alertcard.LevelWarn,
		Title:     fmt.Sprintf("Report for job %q withheld", name),
		Subtitle:  fmt.Sprintf("The node that ran it belongs to <@%s>, not to you.", reply.NodeOwner),
		Text:      "Its reply was neither posted nor kept. Only a node you own, or this gateway itself, can report a job's reply. You are told once until a report gets through again.",
		Detail:    why,
		NextSteps: "Make a node you own the main node (`access.main_node`), or remove the job's `report_to`.",
	}
	if _, _, err := a.postLifecycleAlert(ctx, admin, "", spec); err != nil {
		a.logger.Warn("could not tell the admin a report was withheld", "job", name, "error", err)
	}
}

func (a *Gateway) alreadyWithheld(name string) bool {
	a.confirmedJobsMu.Lock()
	defer a.confirmedJobsMu.Unlock()
	was := a.withheldJobs[name]
	if a.withheldJobs == nil {
		a.withheldJobs = make(map[string]bool)
	}
	a.withheldJobs[name] = true
	return was
}

func (a *Gateway) markReportAllowed(name string) {
	a.confirmedJobsMu.Lock()
	defer a.confirmedJobsMu.Unlock()
	delete(a.withheldJobs, name)
}

func (a *Gateway) postReport(ctx context.Context, dest, text string) (string, error) {
	if a.reports == nil {
		return "", fmt.Errorf("no Slack client is available to post with")
	}
	channel, err := slackclient.ResolveTarget(ctx, a.reports, dest)
	if err != nil {
		return "", err
	}
	for _, chunk := range splitForSlack(text, maxBufferedPostChars) {
		params := slackclient.PostMessageParams{ChannelID: channel, Text: chunk}
		if a.reportBlocks != nil {
			rendered, mentions := replyblock.Rewrite(chunk, nil)
			if blocks, err := a.reportBlocks.Render(rendered, mentions); err == nil {
				params.Blocks = blocks
			}
		}
		if _, err := a.reports.PostMessage(ctx, params); err != nil {
			return channel, err
		}
	}
	return channel, nil
}

func (a *Gateway) recordReply(ctx context.Context, level journal.Level, name, channel, blobRef string, payload map[string]any, state, summary string) {
	if a.recorder == nil {
		return
	}
	payload["state"] = state
	a.recorder.Record(ctx, journal.Event{
		Stream:  journal.StreamJob,
		Kind:    "job.reply",
		Level:   level,
		Summary: summary,
		CorrID:  journal.CorrIDFromContext(ctx),
		Keys:    journal.Keys{JobName: name, ChannelID: channel},
		Payload: payload,
		BlobRef: blobRef,
	})
}
