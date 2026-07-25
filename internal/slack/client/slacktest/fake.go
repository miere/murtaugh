// Package slacktest provides an in-memory SlackAPI fake used by tests across
// the client and tools/slack packages. It exists so each tool package can
// share the same mocking surface without re-implementing the SlackAPI
// methods.
package slacktest

import (
	"context"

	slackgo "github.com/slack-go/slack"

	slacklib "github.com/miere/murtaugh/internal/slack/client"
)

// FakeAPI is an in-memory SlackAPI suitable for tests. Set the *Result /
// *Err fields to control return values; the slice fields capture the calls
// the system under test makes so tests can assert on them.
type FakeAPI struct {
	Channels []slacklib.Channel
	Users    []slacklib.User
	DMFor    map[string]string

	ListChannelsErr error
	ListUsersErr    error
	OpenDMErr       error

	Posted   []slacklib.PostMessageParams
	Updated  []slacklib.UpdateMessageParams
	Deleted  []slacklib.DeleteMessageParams
	Uploaded []slacklib.UploadFileParams
	Created  []slacklib.CreateChannelParams

	PostResult   slacklib.PostMessageResult
	PostErr      error
	UpdateResult slacklib.PostMessageResult
	UpdateErr    error
	DeleteErr    error
	UploadResult slacklib.PostMessageResult
	UploadErr    error
	CreateResult slacklib.CreateChannelResult
	CreateErr    error

	History    []slacklib.Message
	HistoryErr error
	Replies    []slacklib.Message
	RepliesErr error

	HistoryCalls []HistoryCall
	RepliesCalls []RepliesCall

	OpenedViews  []slackgo.ModalViewRequest
	OpenTriggers []string
	OpenViewErr  error

	CanvasEdits   []slacklib.CanvasEditParams
	EditCanvasErr error
	// SectionID is returned by LookupCanvasSection; LookupSectionArgs captures
	// each (canvasID, containsText) it was asked for.
	SectionID         string
	LookupSectionErr  error
	LookupSectionArgs []LookupSectionCall
	// CanvasContent is returned by ReadCanvas; ReadCanvasIDs captures the ids read.
	CanvasContent string
	ReadCanvasErr error
	ReadCanvasIDs []string
}

// LookupSectionCall captures one LookupCanvasSection invocation.
type LookupSectionCall struct {
	CanvasID     string
	ContainsText string
}

// HistoryCall captures one GetHistory invocation.
type HistoryCall struct {
	ChannelID string
	OldestTS  string
	Limit     int
}

// RepliesCall captures one GetReplies invocation.
type RepliesCall struct {
	ChannelID string
	ThreadTS  string
	OldestTS  string
}

// PostMessage records p and returns the configured result/err.
func (f *FakeAPI) PostMessage(_ context.Context, p slacklib.PostMessageParams) (slacklib.PostMessageResult, error) {
	f.Posted = append(f.Posted, p)
	return f.PostResult, f.PostErr
}

// DeleteMessage records p and returns the configured err.
func (f *FakeAPI) DeleteMessage(_ context.Context, p slacklib.DeleteMessageParams) error {
	f.Deleted = append(f.Deleted, p)
	return f.DeleteErr
}

// UploadFile records p and returns the configured result/err.
func (f *FakeAPI) UploadFile(_ context.Context, p slacklib.UploadFileParams) (slacklib.PostMessageResult, error) {
	f.Uploaded = append(f.Uploaded, p)
	return f.UploadResult, f.UploadErr
}

// UpdateMessage records p and returns the configured result/err.
func (f *FakeAPI) UpdateMessage(_ context.Context, p slacklib.UpdateMessageParams) (slacklib.PostMessageResult, error) {
	f.Updated = append(f.Updated, p)
	return f.UpdateResult, f.UpdateErr
}

// GetHistory records the call and returns the configured history/err.
func (f *FakeAPI) GetHistory(_ context.Context, channelID, oldestTS string, limit int) ([]slacklib.Message, error) {
	f.HistoryCalls = append(f.HistoryCalls, HistoryCall{ChannelID: channelID, OldestTS: oldestTS, Limit: limit})
	return f.History, f.HistoryErr
}

// GetReplies records the call and returns the configured replies/err.
func (f *FakeAPI) GetReplies(_ context.Context, channelID, threadTS, oldestTS string) ([]slacklib.Message, error) {
	f.RepliesCalls = append(f.RepliesCalls, RepliesCall{ChannelID: channelID, ThreadTS: threadTS, OldestTS: oldestTS})
	return f.Replies, f.RepliesErr
}

// ListChannels returns the configured channels/err.
func (f *FakeAPI) ListChannels(_ context.Context) ([]slacklib.Channel, error) {
	return f.Channels, f.ListChannelsErr
}

// ListUsers returns the configured users/err.
func (f *FakeAPI) ListUsers(_ context.Context) ([]slacklib.User, error) {
	return f.Users, f.ListUsersErr
}

// OpenDM returns the configured DM for the user or the configured err.
func (f *FakeAPI) OpenDM(_ context.Context, userID string) (string, error) {
	if f.OpenDMErr != nil {
		return "", f.OpenDMErr
	}
	return f.DMFor[userID], nil
}

// CreateChannel records p and returns the configured result/err.
func (f *FakeAPI) CreateChannel(_ context.Context, p slacklib.CreateChannelParams) (slacklib.CreateChannelResult, error) {
	f.Created = append(f.Created, p)
	return f.CreateResult, f.CreateErr
}

// OpenView records the trigger ID and view, then returns the configured err.
func (f *FakeAPI) OpenView(_ context.Context, triggerID string, view slackgo.ModalViewRequest) error {
	f.OpenTriggers = append(f.OpenTriggers, triggerID)
	f.OpenedViews = append(f.OpenedViews, view)
	return f.OpenViewErr
}

// EditCanvas records p and returns the configured err.
func (f *FakeAPI) EditCanvas(_ context.Context, p slacklib.CanvasEditParams) error {
	f.CanvasEdits = append(f.CanvasEdits, p)
	return f.EditCanvasErr
}

// LookupCanvasSection records the query and returns the configured SectionID/err.
func (f *FakeAPI) LookupCanvasSection(_ context.Context, canvasID, containsText string) (string, error) {
	f.LookupSectionArgs = append(f.LookupSectionArgs, LookupSectionCall{CanvasID: canvasID, ContainsText: containsText})
	return f.SectionID, f.LookupSectionErr
}

// ReadCanvas records the id and returns the configured CanvasContent/err.
func (f *FakeAPI) ReadCanvas(_ context.Context, canvasID string) (string, error) {
	f.ReadCanvasIDs = append(f.ReadCanvasIDs, canvasID)
	return f.CanvasContent, f.ReadCanvasErr
}

// LazyClient returns a slacklib.LazyClient that yields f.
func (f *FakeAPI) LazyClient() *slacklib.LazyClient {
	return slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return f, nil })
}
