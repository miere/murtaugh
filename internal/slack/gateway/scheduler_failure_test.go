package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/config"
)

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
	if !strings.Contains(msg.postText, "nightly") || !strings.Contains(msg.postText, "agent delegation is unavailable") {
		t.Fatalf("the alert named neither the job nor the reason:\n%s", msg.postText)
	}
	if !strings.Contains(msg.postText, "jobs run") {
		t.Fatalf("the alert did not offer `jobs run`:\n%s", msg.postText)
	}
}

// A per-minute job that starts failing would otherwise DM the admin fourteen
// hundred times a day.
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

	current = nil
	gw.runScheduledJob(context.Background(), "nightly")
	current = errors.New("something else")
	gw.runScheduledJob(context.Background(), "nightly")
	if msg.postCalls != 2 {
		t.Fatalf("a failure after a recovery produced %d alerts in total; the alert never re-armed", msg.postCalls)
	}
}

// A nightly "your job ran" message trains the admin to ignore the alert that
// matters.
func TestASuccessfulScheduledRunSaysNothing(t *testing.T) {
	gw, msg := failingGateway(t, nil)
	gw.runScheduledJob(context.Background(), "nightly")
	if msg.postCalls != 0 {
		t.Fatalf("a successful run posted %d messages", msg.postCalls)
	}
}

// No admin claimed yet is a fresh install, not an error.
func TestAFailedRunWithNoAdminIsSilentRatherThanBroken(t *testing.T) {
	gw, msg := failingGateway(t, errors.New("boom"))
	gw.cfg = config.AccessConfig{}
	gw.runScheduledJob(context.Background(), "nightly")
	if msg.postCalls != 0 {
		t.Fatalf("a gateway with no admin posted %d messages", msg.postCalls)
	}
}
