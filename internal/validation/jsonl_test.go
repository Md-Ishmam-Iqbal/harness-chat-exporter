package validation

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/domain"
)

func TestSessionsJSONLRequiresGroupedEventsAndReconcilesCounts(t *testing.T) {
	session := domain.SessionRecord{RecordType: domain.RecordSession, SchemaVersion: domain.SchemaVersion, ID: "hh_ses_0123456789abcdefabcd", Harness: "codex", Source: domain.Source{Path: "/source"}, Counts: domain.RecordCounts{Events: 1, UserMessages: 1}}
	event := domain.EventRecord{RecordType: domain.RecordEvent, SchemaVersion: domain.SchemaVersion, ID: "hh_evt_0123456789abcdefabcd", SessionID: session.ID, Sequence: 0, Type: domain.EventUserMessage, Role: domain.RoleUser, Native: json.RawMessage("null")}
	var valid bytes.Buffer
	json.NewEncoder(&valid).Encode(session)
	json.NewEncoder(&valid).Encode(event)
	stats, err := SessionsJSONL(&valid)
	if err != nil || stats.Sessions != 1 || stats.Events != 1 || len(stats.Sources) != 1 {
		t.Fatalf("unexpected stats %#v: %v", stats, err)
	}

	var invalid bytes.Buffer
	json.NewEncoder(&invalid).Encode(event)
	if _, err := SessionsJSONL(&invalid); err == nil {
		t.Fatal("accepted event without preceding session")
	}
	session.Counts.Events = 2
	invalid.Reset()
	json.NewEncoder(&invalid).Encode(session)
	json.NewEncoder(&invalid).Encode(event)
	if _, err := SessionsJSONL(&invalid); err == nil {
		t.Fatal("accepted mismatched session event count")
	}
}
