package askcard

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	slacklib "github.com/miere/murtaugh/internal/slack/client"
)

// Flow posts ask cards and drives each one to a terminal state. A single
// instance is shared between the `ask` tool (which calls Ask and blocks) and the
// gateway (which routes clicks into it).
type Flow struct {
	client *slacklib.LazyClient
	cards  *Renderer

	mu       sync.Mutex
	sessions map[string]*session
}

// New builds a Flow.
func New(client *slacklib.LazyClient, cards *Renderer) *Flow {
	return &Flow{client: client, cards: cards, sessions: make(map[string]*session)}
}

// session is the per-ask rendezvous a click resolves into. It holds the spec so
// an incomplete submit can be validated and the card re-rendered without the
// gateway having to carry any of it, and it accumulates the answers given so far.
type session struct {
	spec Spec

	// channel/ts identify the posted card. They are only known once
	// PostMessage returns, but the session is registered BEFORE that (see Ask),
	// so a click can find the session while these are still empty. posted is
	// closed once they are set; readers on the click path wait for it.
	channel string
	ts      string
	posted  chan struct{}

	mu       sync.Mutex
	resolved bool
	// answers is every answer the user has given across all submissions, not
	// just the last one. See absorb.
	answers map[string][]string
	done    chan Response
}

// setMessage records where the card was posted and releases anyone waiting on
// it. Called exactly once, from Ask, immediately after PostMessage returns.
func (s *session) setMessage(channel, ts string) {
	s.mu.Lock()
	s.channel, s.ts = channel, ts
	s.mu.Unlock()
	close(s.posted)
}

// message returns the card's coordinates, blocking until Ask has recorded them.
// The wait is bounded by ctx and is in practice instantaneous: Slack cannot have
// delivered the card to the clicker before PostMessage returned to us.
func (s *session) message(ctx context.Context) (channel, ts string, err error) {
	select {
	case <-s.posted:
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.channel, s.ts, nil
}

// absorb folds one submission's answers into the running set and returns the
// merged result.
//
// A submission is a DELTA, not the whole picture. After the validation
// re-render, Slack does not necessarily report the state of inputs the user has
// not touched since that update — the pre-ticked options are visibly there, but
// they need not come back in state.values. Treating each submit as complete
// therefore lost every earlier answer, so filling in the one missing question
// and pressing Submit again reported the *other* questions as missing: an
// unwinnable form that rejected the user no matter what they did.
//
// Merging also makes the flow correct if Slack does report the full state — the
// merge is then simply idempotent — so it does not depend on which behaviour is
// in play.
//
// Only non-empty answers overwrite. An input that arrives with nothing selected
// is indistinguishable from one that was not reported at all, so the prior
// answer is kept rather than silently cleared; a radio button cannot be cleared
// by hand anyway, and for checkboxes keeping the earlier answer is the kinder
// failure.
func (s *session) absorb(submitted map[string][]string) map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.answers == nil {
		s.answers = make(map[string][]string, len(submitted))
	}
	for key, choices := range submitted {
		if len(choices) == 0 {
			continue
		}
		s.answers[key] = choices
	}
	merged := make(map[string][]string, len(s.answers))
	for key, choices := range s.answers {
		merged[key] = append([]string(nil), choices...)
	}
	return merged
}

// resolve delivers a response once. Later clicks on an already-answered card —
// a double-click, or a second person pressing Submit — are dropped rather than
// racing, and report false so the caller can stay quiet.
func (s *session) resolve(r Response) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resolved {
		return false
	}
	s.resolved = true
	s.done <- r
	return true
}

// Ask posts the card and blocks until the user answers, asks to chat, the wait
// times out, or ctx is cancelled. It always rewrites the card to a terminal,
// input-less state before returning.
func (f *Flow) Ask(ctx context.Context, dest Destination, spec Spec) (Response, error) {
	if strings.TrimSpace(dest.ChannelID) == "" {
		return Response{}, fmt.Errorf("askcard: no Slack channel to ask in")
	}
	if len(spec.Questions) == 0 {
		return Response{}, fmt.Errorf("askcard: no questions to ask")
	}
	for i := range spec.Questions {
		q := &spec.Questions[i]
		if strings.TrimSpace(q.Key) == "" {
			q.Key = fmt.Sprintf("q%d", i)
		}
		if len(q.Options) == 0 {
			return Response{}, fmt.Errorf("askcard: question %q has no options", q.Key)
		}
	}

	api, err := f.client.Get()
	if err != nil {
		return Response{}, err
	}
	corr, err := newCorrelationID()
	if err != nil {
		return Response{}, err
	}

	blocks, err := f.cards.render(PendingTemplate, f.data(spec, corr, StatePending, "", "", nil))
	if err != nil {
		return Response{}, err
	}

	// Register before the card is posted, never after. The corr id is already
	// baked into the card's buttons, so the instant Slack has the message a
	// click can arrive — and one that lands before this map entry exists is
	// rejected with "no question is waiting", silently discarding an answer the
	// user believes they gave.
	s := &session{spec: spec, done: make(chan Response, 1), posted: make(chan struct{})}
	f.register(corr, s)
	defer f.unregister(corr)

	posted, err := api.PostMessage(ctx, slacklib.PostMessageParams{
		ChannelID: dest.ChannelID,
		ThreadTS:  dest.ThreadTS,
		Text:      fallbackText(spec),
		Blocks:    blocks,
	})
	if err != nil {
		return Response{}, fmt.Errorf("askcard: post question card: %w", err)
	}
	s.setMessage(posted.Channel, posted.TS)

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var resp Response
	var state State
	select {
	case resp = <-s.done:
		state = StateAnswered
		if resp.Chat {
			state = StateChat
		}
	case <-timer.C:
		resp, state = Response{TimedOut: true}, StateTimeout
	case <-ctx.Done():
		resp, state = Response{Cancelled: true}, StateCancelled
	}

	f.settle(spec, corr, state, resp, posted.Channel, posted.TS)
	return resp, nil
}

// settle rewrites the card to its terminal state. Best-effort and on a fresh
// context: the ctx that drove Ask may already be cancelled on the interrupt
// path, and a card left showing live inputs would be worse than a missed update.
func (f *Flow) settle(spec Spec, corr string, state State, resp Response, channel, ts string) {
	if channel == "" || ts == "" {
		return
	}
	api, err := f.client.Get()
	if err != nil {
		return
	}
	blocks, err := f.cards.render(templateFor(state), f.data(spec, corr, state, "", resp.UserID, resp.Answers))
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = api.UpdateMessage(ctx, slacklib.UpdateMessageParams{
		ChannelID: channel,
		TS:        ts,
		Text:      fallbackText(spec),
		Blocks:    blocks,
	})
}

// HandleClick routes a click into the waiting ask.
//
// Submit is the only path that can decline to resolve: Slack does not enforce
// required inputs in a message, so an incomplete submission re-renders the card
// with a callout and the user's existing selections restored, leaving the ask
// blocked. Chat always resolves.
func (f *Flow) HandleClick(ctx context.Context, corr string, action Action, userID string, answers map[string][]string) error {
	s, ok := f.session(corr)
	if !ok {
		return fmt.Errorf("askcard: no question is waiting for %q", corr)
	}

	switch action {
	case ActionChat:
		s.resolve(Response{Chat: true, Submitted: true, UserID: userID})
		return nil

	case ActionSubmit:
		// Merge before validating: what the user has told us is the accumulation
		// of every submission, not whatever this one happened to carry.
		merged := s.absorb(answers)
		if missing := unanswered(s.spec.Questions, merged); len(missing) > 0 {
			return f.reprompt(ctx, corr, s, merged, missing)
		}
		s.resolve(Response{Submitted: true, UserID: userID, Answers: merged})
		return nil
	}
	return fmt.Errorf("askcard: unknown ask action %q", action)
}

// reprompt re-renders the pending card with a validation callout, carrying the
// answers already given back into the inputs. Without that carry-over a user who
// answered three of four questions would watch all three reset the moment the
// callout appeared, which reads as the bot discarding their work.
func (f *Flow) reprompt(ctx context.Context, corr string, s *session, answers map[string][]string, missing []Question) error {
	api, err := f.client.Get()
	if err != nil {
		return err
	}
	channel, ts, err := s.message(ctx)
	if err != nil {
		return err
	}
	blocks, err := f.cards.render(PendingTemplate,
		f.data(s.spec, corr, StatePending, validationMessage(missing), "", answers))
	if err != nil {
		return err
	}
	_, err = api.UpdateMessage(ctx, slacklib.UpdateMessageParams{
		ChannelID: channel,
		TS:        ts,
		Text:      fallbackText(s.spec),
		Blocks:    blocks,
	})
	return err
}

// validationMessage names what is still missing. Listing the numbers beats a
// generic "please answer everything" on a card of four questions where three are
// already done.
func validationMessage(missing []Question) string {
	if len(missing) == 1 {
		return fmt.Sprintf("Question %s still needs an answer — or press “Chat About This”.",
			questionNumbers(missing))
	}
	return fmt.Sprintf("Questions %s still need an answer — or press “Chat About This”.",
		questionNumbers(missing))
}

// questionNumbers renders the missing questions' 1-based positions as "1, 3 and
// 4". The keys are assigned as q0, q1, … in order, so the suffix is the index.
func questionNumbers(missing []Question) string {
	nums := make([]string, 0, len(missing))
	for _, q := range missing {
		nums = append(nums, strings.TrimPrefix(q.Key, "q"))
	}
	for i, n := range nums {
		if v, err := parsePositiveInt(n); err == nil {
			nums[i] = fmt.Sprint(v + 1)
		}
	}
	if len(nums) == 1 {
		return nums[0]
	}
	return strings.Join(nums[:len(nums)-1], ", ") + " and " + nums[len(nums)-1]
}

func parsePositiveInt(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

// data builds the template context for one render. selected carries the answers
// to pre-tick (the validation re-render) or to display (a terminal card); nil
// leaves every option unselected.
func (f *Flow) data(spec Spec, corr string, state State, validationError, answeredBy string, selected map[string][]string) cardData {
	questions := make([]questionData, 0, len(spec.Questions))
	for i, q := range spec.Questions {
		chosen := selected[q.Key]
		opts := make([]optionData, 0, len(q.Options))
		for _, o := range q.Options {
			opts = append(opts, optionData{
				Value:    o.Label,
				Text:     o.optionText(),
				Selected: containsLabel(chosen, o.Label),
			})
		}
		questions = append(questions, questionData{
			Key:         q.Key,
			BlockID:     inputPrefix + q.Key,
			ActionID:    inputPrefix + q.Key,
			Label:       q.label(i),
			MultiSelect: q.MultiSelect,
			Options:     opts,
			Answers:     chosen,
		})
	}

	title := strings.TrimSpace(spec.Title)
	if title == "" {
		title = "User Input Required"
	}
	missing := 0
	if validationError != "" {
		missing = len(unanswered(spec.Questions, selected))
	}
	return cardData{
		Title:           title,
		Subtitle:        subtitleFor(state, len(spec.Questions), missing),
		State:           string(state),
		Questions:       questions,
		ValidationError: validationError,
		ShowActions:     state == StatePending,
		AnsweredBy:      answeredBy,
		ActionSubmit:    ActionID(corr, ActionSubmit),
		ActionChat:      ActionID(corr, ActionChat),
	}
}

func containsLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

func (f *Flow) register(corr string, s *session) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[corr] = s
}

func (f *Flow) unregister(corr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, corr)
}

func (f *Flow) session(corr string) (*session, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[corr]
	return s, ok
}
