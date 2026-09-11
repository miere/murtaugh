package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/config"
)

// A failed scheduled run used to write one line to slack.err.log and stop
// there, and the only detector that alerts — reportMissedJobs — could not see
// it: the occurrence claim is taken BEFORE the run and never released, so a job
// that claimed its slot and then failed reads as one that succeeded. #170's
// split makes that sharper rather than softer, because "agent delegation is
// unavailable" is a way for every job on a gateway to fail at once, and #199 is
// explicit that landing the split without an answer fails silently.

func failingGateway(t *testing.T, runErr error) (*Gateway, *recordingMessaging) {
	t.Helper()
	msg := &recordingMessaging{}
	gw := &Gateway{
		logger:        newSilentLogger(),
		messaging:     msg,
		cfg:           config.AccessConfig{AdminUser: "U-admin"},
		scheduledJobs: map[string]config.JobProfile{"nightly": {Schedule: "0 3 * * *"}},
		runJob: func(context.Context, string) (*agentruntime.Reply, error) {
			return nil, runErr
		},
	}
	return gw, msg
}

func TestAFailedScheduledRunTellsTheAdmin(t *testing.T) {
	gw, msg := failingGateway(t, errors.New("agent delegation is unavailable"))
	gw.runScheduledJob(context.Background(), "nightly")

	if msg.postCalls != 1 {
		t.Fatalf("a failed scheduled run produced %d messages; a cron that stops working and tells nobody is the failure this is for", msg.postCalls)
	}
	// The job and the reason both have to be in it: an alert that says only
	// "a job failed" sends the reader back to the log it was meant to replace.
	if !strings.Contains(msg.postText, "nightly") || !strings.Contains(msg.postText, "agent delegation is unavailable") {
		t.Fatalf("the alert named neither the job nor the reason:\n%s", msg.postText)
	}
	// The same remedy a missed occurrence offers. Two alerts about the same
	// situation must not send the admin to two different places.
	if !strings.Contains(msg.postText, "jobs run") {
		t.Fatalf("the alert did not offer `jobs run`:\n%s", msg.postText)
	}
}

// Once per run of failures, not once per occurrence. A per-minute job that
// starts failing would otherwise DM the admin fourteen hundred times a day,
// which is a way of telling them nothing.
func TestRepeatedFailuresAlertOnceAndRearmOnRecovery(t *testing.T) {
	failing := errors.New("boom")
	var current error = failing
	msg := &recordingMessaging{}
	gw := &Gateway{
		logger:        newSilentLogger(),
		messaging:     msg,
		cfg:           config.AccessConfig{AdminUser: "U-admin"},
		scheduledJobs: map[string]config.JobProfile{"nightly": {Schedule: "* * * * *"}},
		runJob:        func(context.Context, string) (*agentruntime.Reply, error) { return nil, current },
	}

	gw.runScheduledJob(context.Background(), "nightly")
	gw.runScheduledJob(context.Background(), "nightly")
	gw.runScheduledJob(context.Background(), "nightly")
	if msg.postCalls != 1 {
		t.Fatalf("three consecutive failures produced %d alerts", msg.postCalls)
	}

	// It recovers, then fails again a month later for a different reason. That
	// one is news and has to be reported.
	current = nil
	gw.runScheduledJob(context.Background(), "nightly")
	current = errors.New("something else")
	gw.runScheduledJob(context.Background(), "nightly")
	if msg.postCalls != 2 {
		t.Fatalf("a failure after a recovery produced %d alerts in total; the alert never re-armed", msg.postCalls)
	}
}

// A successful run says nothing at all. The alert is for the thing that did not
// happen, and a nightly "your job ran" trains the admin to ignore the one that
// matters — the same argument that keeps node disconnects journalled rather than
// announced.
func TestASuccessfulScheduledRunSaysNothing(t *testing.T) {
	gw, msg := failingGateway(t, nil)
	gw.runScheduledJob(context.Background(), "nightly")
	if msg.postCalls != 0 {
		t.Fatalf("a successful run posted %d messages", msg.postCalls)
	}
}

// No admin claimed yet is a fresh install, not an error. It must not panic and
// must not post into the void.
func TestAFailedRunWithNoAdminIsSilentRatherThanBroken(t *testing.T) {
	gw, msg := failingGateway(t, errors.New("boom"))
	gw.cfg = config.AccessConfig{}
	gw.runScheduledJob(context.Background(), "nightly")
	if msg.postCalls != 0 {
		t.Fatalf("a gateway with no admin posted %d messages", msg.postCalls)
	}
}
