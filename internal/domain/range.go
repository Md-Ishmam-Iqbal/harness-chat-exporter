package domain

import "time"

type SessionScope string

const (
	ScopeTouched    SessionScope = "touched"
	ScopeEventsOnly SessionScope = "events-only"
)

type TimeRange struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

func (r TimeRange) Valid() bool { return !r.From.After(r.To) }

func (r TimeRange) Contains(t time.Time) bool {
	return !t.Before(r.From) && !t.After(r.To)
}
