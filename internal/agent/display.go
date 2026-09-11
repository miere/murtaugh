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
	// DisplayChat is not a refusal: the user wants to talk the options over
	// before choosing, and the model has to hear it as their question.
	DisplayChat           DisplayOutcome = "chat"
	DisplayNoConversation DisplayOutcome = "no_conversation"
	DisplayUnavailable    DisplayOutcome = "unavailable"
)

// DisplayAnswer answers both kinds of request so every hop between a tool and
// Slack carries one shape back.
type DisplayAnswer struct {
	Outcome DisplayOutcome
	Answers map[string][]string
	Choice  string
	UserID  string
	Note    string
}
