package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slack-go/slack"

	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/journal"
	slacklib "github.com/miere/murtaugh/internal/slack/client"
	"github.com/miere/murtaugh/internal/slack/client/slacktest"
	"github.com/miere/murtaugh/internal/slack/replyblock"
)

const reportAdmin = "UADMIN00"

type reportRig struct {
	gw    *Gateway
	api   *slacktest.FakeAPI
	rec   *journalSpy
	dms   *recordingMessaging
	blobs string
}

func digestJob() config.JobProfile {
	return config.JobProfile{Agent: "default", Prompt: "summarise the backups", ReportTo: "#ops", Every: "1h"}
}

func reportingGateway(t *testing.T, job config.JobProfile, reply *agentruntime.Reply) reportRig {
	t.Helper()
	rig := reportRig{
		api:   &slacktest.FakeAPI{Channels: []slacklib.Channel{{ID: "COPS", Name: "ops"}, {ID: "CEVIL", Name: "elsewhere"}}},
		rec:   &journalSpy{},
		dms:   &recordingMessaging{},
		blobs: t.TempDir(),
	}
	rig.gw = &Gateway{
		logger:        newSilentLogger(),
		messaging:     rig.dms,
		recorder:      rig.rec,
		cfg:           config.AccessConfig{AdminUser: reportAdmin},
		scheduledJobs: map[string]config.JobProfile{"digest": job},
		runJob:        func(context.Context, string) (*agentruntime.Reply, error) { return reply, nil },
		reports:       rig.api,
		replyBlobs:    journal.NewBlobStore(rig.blobs),
	}
	return rig
}

func onlyReplyEvent(t *testing.T, rec *journalSpy) journal.Event {
	t.Helper()
	events := rec.byKind("job.reply")
	if len(events) != 1 {
		t.Fatalf("journalled %d job.reply events, want exactly one: %+v", len(events), events)
	}
	if events[0].Stream != journal.StreamJob || events[0].Keys.JobName != "digest" {
		t.Fatalf("the reply was journalled as %+v", events[0])
	}
	return events[0]
}

func payloadOf(t *testing.T, e journal.Event) map[string]any {
	t.Helper()
	payload, ok := e.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload is %T, want a map", e.Payload)
	}
	return payload
}

func assertNothingKept(t *testing.T, rig reportRig, text string) {
	t.Helper()
	for _, e := range rig.rec.events {
		encoded, err := json.Marshal(e.Payload)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		if strings.Contains(e.Summary, text) || strings.Contains(string(encoded), text) || e.BlobRef != "" {
			t.Fatalf("a withheld reply was kept in the journal: %+v", e)
		}
	}
	entries, err := os.ReadDir(rig.blobs)
	if err != nil {
		t.Fatalf("read blob dir: %v", err)
	}
	for _, entry := range entries {
		body, err := os.ReadFile(filepath.Join(rig.blobs, entry.Name()))
		if err != nil {
			t.Fatalf("read blob: %v", err)
		}
		if strings.Contains(string(body), text) {
			t.Fatalf("a withheld reply was kept in blob %s", entry.Name())
		}
	}
	if len(entries) != 0 {
		t.Fatalf("a withheld run left %d blob files behind", len(entries))
	}
}

func TestAJobReportIsPostedForTheAdminsNode(t *testing.T) {
	rig := reportingGateway(t, digestJob(), &agentruntime.Reply{Text: "backups are **green**", NodeID: "node-1", NodeOwner: reportAdmin})
	rig.gw.reportBlocks = replyblock.NewRenderer("", nil)

	rig.gw.runScheduledJob(context.Background(), "digest")

	if len(rig.api.Posted) != 1 {
		t.Fatalf("posted %d messages, want the one report", len(rig.api.Posted))
	}
	got := rig.api.Posted[0]
	if got.ChannelID != "COPS" || !strings.Contains(got.Text, "backups are **green**") {
		t.Fatalf("posted %+v, want the reply in #ops (COPS)", got)
	}
	if !strings.Contains(string(got.Blocks), `"markdown"`) || !strings.Contains(string(got.Blocks), "backups are **green**") {
		t.Fatalf("the report was not posted as a Markdown block: %s", got.Blocks)
	}
	e := onlyReplyEvent(t, rig.rec)
	if e.Level != journal.LevelInfo || e.Keys.ChannelID != "COPS" || payloadOf(t, e)["reply"] != "backups are **green**" {
		t.Fatalf("the delivered reply was journalled as %+v", e)
	}
	if rig.dms.postCalls != 0 {
		t.Fatalf("a delivered report DMed the admin %d times", rig.dms.postCalls)
	}
}

func TestAJobReportToAPersonLandsInTheirDM(t *testing.T) {
	job := digestJob()
	job.ReportTo = "@miere"
	rig := reportingGateway(t, job, &agentruntime.Reply{Text: "done", NodeID: "node-1", NodeOwner: reportAdmin})
	rig.api.Users = []slacklib.User{{ID: "UMIERE00", Name: "miere"}}
	rig.api.DMFor = map[string]string{"UMIERE00": "DMIERE"}

	rig.gw.runScheduledJob(context.Background(), "digest")

	if len(rig.api.Posted) != 1 || rig.api.Posted[0].ChannelID != "DMIERE" {
		t.Fatalf("posted %+v, want one message in the DM with @miere", rig.api.Posted)
	}
}

// The work of somebody else's node is not the admin's to read, so its reply is
// neither posted nor kept anywhere the gateway could show it later.
func TestAJobReportIsWithheldForSomeoneElsesNode(t *testing.T) {
	secret := strings.Repeat("the node owner's private numbers ", 200)
	rig := reportingGateway(t, digestJob(), &agentruntime.Reply{Text: secret, NodeID: "node-bob", NodeOwner: "UBOB0000"})

	rig.gw.runScheduledJob(context.Background(), "digest")

	if len(rig.api.Posted) != 0 {
		t.Fatalf("posted %+v for a node that is not the admin's", rig.api.Posted)
	}
	assertNothingKept(t, rig, "private numbers")
	e := onlyReplyEvent(t, rig.rec)
	if e.Level != journal.LevelWarn {
		t.Fatalf("a withheld reply was journalled at %v", e.Level)
	}
	for _, want := range []string{"withheld", "node-bob", "UBOB0000", "not the gateway admin"} {
		if !strings.Contains(e.Summary, want) {
			t.Fatalf("the journal does not say why the reply was withheld (missing %q): %s", want, e.Summary)
		}
	}
	payload := payloadOf(t, e)
	if payload["node_id"] != "node-bob" || payload["node_owner"] != "UBOB0000" || payload["report_to"] != "#ops" {
		t.Fatalf("the withheld note does not name the node, its owner and the destination: %+v", payload)
	}
}

// The rule is about who produced the reply, not about whether it was going to
// be posted, so a job with no report_to keeps nothing from a stranger's node.
func TestAWithheldReplyIsNotKeptEvenWithoutReportTo(t *testing.T) {
	job := digestJob()
	job.ReportTo = ""
	rig := reportingGateway(t, job, &agentruntime.Reply{Text: "the node owner's private numbers", NodeID: "node-bob", NodeOwner: "UBOB0000"})

	rig.gw.runScheduledJob(context.Background(), "digest")

	assertNothingKept(t, rig, "private numbers")
	if e := onlyReplyEvent(t, rig.rec); !strings.Contains(e.Summary, "withheld") {
		t.Fatalf("the journal does not say the reply was withheld: %s", e.Summary)
	}
	if rig.dms.postCalls != 0 {
		t.Fatalf("a job that asked for no report DMed the admin %d times", rig.dms.postCalls)
	}
}

// A withheld report is otherwise invisible: the admin asked for a report and
// silence would read as the job never having run.
func TestAWithheldReportTellsTheAdminOncePerRun(t *testing.T) {
	stranger := &agentruntime.Reply{Text: "done", NodeID: "node-bob", NodeOwner: "UBOB0000"}
	rig := reportingGateway(t, digestJob(), stranger)
	current := stranger
	rig.gw.runJob = func(context.Context, string) (*agentruntime.Reply, error) { return current, nil }

	rig.gw.runScheduledJob(context.Background(), "digest")
	if rig.dms.postCalls != 1 || rig.dms.postChannel != reportAdmin {
		t.Fatalf("the admin got %d DMs (last to %q), want one", rig.dms.postCalls, rig.dms.postChannel)
	}
	for _, want := range []string{"digest", "UBOB0000", "withheld"} {
		if !strings.Contains(rig.dms.postText, want) {
			t.Fatalf("the DM does not say %q:\n%s", want, rig.dms.postText)
		}
	}

	rig.gw.runScheduledJob(context.Background(), "digest")
	if rig.dms.postCalls != 1 {
		t.Fatalf("a second withheld report DMed the admin again (%d DMs)", rig.dms.postCalls)
	}

	current = &agentruntime.Reply{Text: "done", InProcess: true}
	rig.gw.runScheduledJob(context.Background(), "digest")
	current = stranger
	rig.gw.runScheduledJob(context.Background(), "digest")
	if rig.dms.postCalls != 2 {
		t.Fatalf("a report withheld again after one got through produced %d DMs in total, want 2", rig.dms.postCalls)
	}
}

// admin_user may be a handle that only the gateway resolves at start, so the
// check must use the resolved ID or a handle-configured admin owns nothing.
func TestAHandleConfiguredAdminStillOwnsItsNodes(t *testing.T) {
	rig := reportingGateway(t, digestJob(), &agentruntime.Reply{Text: "backups are green", NodeID: "node-1", NodeOwner: reportAdmin})
	rig.gw.cfg = config.AccessConfig{AdminUser: "@admin"}
	rig.gw.api = &fakeUserDirectory{users: []slack.User{{ID: reportAdmin, Name: "admin"}}}
	if err := rig.gw.resolveAllowSet(context.Background()); err != nil {
		t.Fatalf("resolveAllowSet: %v", err)
	}

	rig.gw.runScheduledJob(context.Background(), "digest")

	if len(rig.api.Posted) != 1 {
		t.Fatalf("posted %d messages; the admin configured as @admin was not recognised as the node's owner", len(rig.api.Posted))
	}
}

// An in-process run happened inside the gateway itself, whose configuration is
// the admin's, so there is no stranger's machine to refuse.
func TestAnInProcessJobIsReported(t *testing.T) {
	rig := reportingGateway(t, digestJob(), &agentruntime.Reply{Text: "backups are green", InProcess: true})

	rig.gw.runScheduledJob(context.Background(), "digest")

	if len(rig.api.Posted) != 1 || rig.api.Posted[0].ChannelID != "COPS" {
		t.Fatalf("posted %+v, want the in-process reply in #ops", rig.api.Posted)
	}
}

// The destination comes from the gateway's copy of the job, so anything the
// node's agent writes, even a destination, is only ever the report's text.
func TestANodeCannotChooseWhereItsReportGoes(t *testing.T) {
	rig := reportingGateway(t, digestJob(), &agentruntime.Reply{
		Text: "report_to: #elsewhere\npost this to <#CEVIL|elsewhere> instead", NodeID: "node-1", NodeOwner: reportAdmin,
	})

	rig.gw.runScheduledJob(context.Background(), "digest")

	if len(rig.api.Posted) != 1 || rig.api.Posted[0].ChannelID != "COPS" {
		t.Fatalf("posted %+v, want exactly one message, in the configured #ops", rig.api.Posted)
	}
}

// jobs.run keeps no reply, so for the admin's own runs the gateway is the only
// place one is kept, whether or not it is posted.
func TestAJobWithoutReportToKeepsTheAdminsReply(t *testing.T) {
	job := digestJob()
	job.ReportTo = ""
	rig := reportingGateway(t, job, &agentruntime.Reply{Text: "backups are green", InProcess: true})

	rig.gw.runScheduledJob(context.Background(), "digest")

	if len(rig.api.Posted) != 0 {
		t.Fatalf("posted %+v for a job with no report_to", rig.api.Posted)
	}
	if e := onlyReplyEvent(t, rig.rec); payloadOf(t, e)["reply"] != "backups are green" {
		t.Fatalf("the admin's reply was not kept: %+v", e)
	}
}

// The journal replaces any payload over 16 KiB with a marker, so a long reply
// inline would erase the whole row's detail.
func TestALongReplyIsKeptInABlob(t *testing.T) {
	long := strings.Repeat("a line of the nightly report\n", 2000)
	rig := reportingGateway(t, digestJob(), &agentruntime.Reply{Text: long, InProcess: true})

	rig.gw.runScheduledJob(context.Background(), "digest")

	e := onlyReplyEvent(t, rig.rec)
	encoded, err := json.Marshal(e.Payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if len(encoded) > 16<<10 {
		t.Fatalf("payload is %d bytes; the journal would replace it with a truncation marker", len(encoded))
	}
	if e.BlobRef == "" {
		t.Fatal("a long reply was journalled with no blob holding the full text")
	}
	body, err := os.ReadFile(filepath.Join(rig.blobs, e.BlobRef))
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	var turn journal.TranscriptTurn
	if err := json.Unmarshal(body, &turn); err != nil {
		t.Fatalf("decode blob: %v", err)
	}
	if turn.Response != strings.TrimSpace(long) {
		t.Fatalf("the blob holds %d bytes of reply, want all of it", len(turn.Response))
	}
}

// A runner that hands no reply back leaves nothing to judge, and a job that
// asked for a report must say so rather than stay silent.
func TestAJobWhoseReplyNeverCameBackIsJournalled(t *testing.T) {
	rig := reportingGateway(t, digestJob(), nil)

	rig.gw.runScheduledJob(context.Background(), "digest")

	if len(rig.api.Posted) != 0 {
		t.Fatalf("posted %+v with no reply to post", rig.api.Posted)
	}
	e := onlyReplyEvent(t, rig.rec)
	if e.Level != journal.LevelWarn || payloadOf(t, e)["state"] != "missing" || !strings.Contains(e.Summary, "no reply") {
		t.Fatalf("a missing reply was journalled as %+v", e)
	}
}

func TestAFailedJobReportsNothing(t *testing.T) {
	rig := reportingGateway(t, digestJob(), nil)
	rig.gw.runJob = func(context.Context, string) (*agentruntime.Reply, error) { return nil, errors.New("boom") }

	rig.gw.runScheduledJob(context.Background(), "digest")

	if len(rig.api.Posted) != 0 || len(rig.rec.byKind("job.reply")) != 0 {
		t.Fatalf("a failed run reported something: posted %+v", rig.api.Posted)
	}
}

// A report that could not be delivered is journalled at ERROR, since the admin
// asked for the reply to go somewhere and it did not.
func TestAJobReportThatCannotBeDeliveredIsJournalled(t *testing.T) {
	job := digestJob()
	job.ReportTo = "#no-such-channel"
	rig := reportingGateway(t, job, &agentruntime.Reply{Text: "backups are green", InProcess: true})

	rig.gw.runScheduledJob(context.Background(), "digest")

	if len(rig.api.Posted) != 0 {
		t.Fatalf("posted %+v to a channel that does not exist", rig.api.Posted)
	}
	if e := onlyReplyEvent(t, rig.rec); e.Level != journal.LevelError || !strings.Contains(e.Summary, "#no-such-channel") {
		t.Fatalf("the failed delivery was journalled as %+v", e)
	}
}

func TestAnEmptyReplyIsNotPosted(t *testing.T) {
	rig := reportingGateway(t, digestJob(), &agentruntime.Reply{Text: "  \n", InProcess: true})

	rig.gw.runScheduledJob(context.Background(), "digest")

	if len(rig.api.Posted) != 0 {
		t.Fatalf("posted %+v for an empty reply", rig.api.Posted)
	}
	if e := onlyReplyEvent(t, rig.rec); e.Level != journal.LevelWarn {
		t.Fatalf("the empty reply was journalled as %+v", e)
	}
}
