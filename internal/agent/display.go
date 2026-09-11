package agent

// QuestionRequest names no conversation on purpose: whoever draws it binds it to
// the conversation the request came from, so a tool cannot aim it elsewhere.
type QuestionRequest struct {
	Title     string
	Questions []Question
}

type Question struct {
	Key         string
	Header      string
	Question    string
	Options     []QuestionOption
	MultiSelect bool
}

type QuestionOption struct {
	Label       string
	Description string
}

// Plain is shared by the side that draws a request and the tool that shapes its
// answer, so both agree on whether a row of buttons was enough.
func (r QuestionRequest) Plain() bool {
	if len(r.Questions) != 1 {
		return false
	}
	q := r.Questions[0]
	if q.MultiSelect || q.Header != "" {
		return false
	}
	for _, o := range q.Options {
		if o.Description != "" {
			return false
		}
	}
	return true
}

// PlanRequest names no conversation, for the same reason QuestionRequest does not.
type PlanRequest struct {
	Title string
	Plan  string
}

const (
	PlanProceed = "proceed"
	PlanRevise  = "revise"
	PlanCancel  = "cancel"
)

type DisplayOutcome string

const (
	DisplayAnswered  DisplayOutcome = "answered"
	DisplayTimedOut  DisplayOutcome = "timed_out"
	DisplayDismissed DisplayOutcome = "dismissed"
	DisplayDenied    DisplayOutcome = "denied"
	// DisplayApproved lets a sign-in's command start; the sign-in stays open for
	// its link and code.
	DisplayApproved DisplayOutcome = "approved"
	// DisplayChat is not a refusal: the user wants to talk the options over
	// before choosing, and the model has to hear it as their question.
	DisplayChat           DisplayOutcome = "chat"
	DisplayNoConversation DisplayOutcome = "no_conversation"
	DisplayUnavailable    DisplayOutcome = "unavailable"
)

// DisplayAnswer answers every kind of request so each hop between a tool and
// Slack carries one shape back.
type DisplayAnswer struct {
	Outcome DisplayOutcome
	Answers map[string][]string
	Choice  string
	UserID  string
	Note    string
	Code    string
}

// SignInRequest names no conversation and carries no environment: the node
// runs the sign-in in its own, and the gateway only draws it.
type SignInRequest struct {
	Tool      string
	Profile   string
	URL       string
	NeedsCode bool
	// Command is set when the owner has to approve what runs before it starts,
	// so the request carries no URL until they have.
	Command string
}

type SignInState string

const (
	SignInWorking   SignInState = "working"
	SignInReady     SignInState = "ready"
	SignInSuccess   SignInState = "success"
	SignInFailed    SignInState = "failed"
	SignInTimedOut  SignInState = "timeout"
	SignInCancelled SignInState = "cancelled"

	// SignInConfirming holds a finished sign-in until the gateway confirms its
	// owner still has access, because losing access must win over finishing.
	SignInConfirming SignInState = "confirming"
)

// Terminal is shared by the node and the gateway so both stop listening for a
// sign-in at the same moment.
func (s SignInState) Terminal() bool {
	switch s {
	case SignInSuccess, SignInFailed, SignInTimedOut, SignInCancelled:
		return true
	}
	return false
}
