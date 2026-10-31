package action

import (
	"slices"
	"testing"
	"testing/synctest"
)

func TestProviderEventQueueDoesNotHoldCLIReaderOnStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var sent []string
		queue := newProviderEventQueue(func(event attemptRecord) error {
			if len(sent) == 0 {
				close(entered)
				<-release
			}
			sent = append(sent, event.Text)
			return nil
		})
		queue.add(attemptRecord{Agent: "native", Text: "first"})
		<-entered
		returned := make(chan struct{})
		go func() {
			queue.add(attemptRecord{Agent: "native", Text: "second"})
			close(returned)
		}()
		synctest.Wait()
		select {
		case <-returned:
		default:
			t.Fatal("CLI reader blocked behind the Status sender")
		}
		close(release)
		if !queue.finish() || !slices.Equal(sent, []string{"first", "second"}) {
			t.Fatalf("ordered flush failed: sent=%v", sent)
		}
	})
}

func TestProviderEventQueueReportsGapsAndBoundedBacklog(t *testing.T) {
	for _, test := range []struct {
		name  string
		add   []attemptRecord
		limit int64
	}{
		{"missing native identity", []attemptRecord{{Text: "unbound"}}, maxAttemptRecordBytes},
		{"unbound ordinary diagnostic", []attemptRecord{{Kind: attemptRecordDiagnostic, Provider: "codex", EventSource: "codex.other", Text: "unbound"}}, maxAttemptRecordBytes},
		{"backlog cap", []attemptRecord{{Agent: "native", Text: "too large"}}, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			var sent []attemptRecord
			queue := newProviderEventQueue(func(event attemptRecord) error {
				sent = append(sent, event)
				return nil
			})
			queue.limit = test.limit
			for _, event := range test.add {
				queue.add(event)
			}
			if queue.finish() || len(sent) != 0 {
				t.Fatalf("gap falsely marked complete or sent: %+v", sent)
			}
		})
	}
}

func TestProviderEventQueueRetainsCallFailureWithoutClaimingCoverage(t *testing.T) {
	for _, test := range []struct{ source, text string }{
		{"codex.call.failed", "failed"},
		{"codex.call.diagnostic_unavailable", ""},
	} {
		t.Run(test.source, func(t *testing.T) {
			var sent []attemptRecord
			queue := newProviderEventQueue(func(event attemptRecord) error {
				sent = append(sent, event)
				return nil
			})
			queue.add(attemptRecord{Kind: attemptRecordDiagnostic, Provider: "codex", EventSource: test.source, Text: test.text})
			queue.add(attemptRecord{Kind: attemptRecordDiscussion, Agent: "native", Text: "later"})
			if queue.finish() || len(sent) != 2 || sent[0].Agent != "" || sent[0].Text != test.text || sent[1].Text != "later" {
				t.Fatalf("call failure was lost or falsely proved coverage: %+v", sent)
			}
		})
	}
}

func TestProviderEventQueueKeepsLaterValidEventsAfterGap(t *testing.T) {
	var sent []string
	queue := newProviderEventQueue(func(event attemptRecord) error {
		sent = append(sent, event.Text)
		return nil
	})
	queue.add(attemptRecord{Text: "unbound"})
	queue.add(attemptRecord{Agent: "native", Text: "later"})
	if queue.finish() || !slices.Equal(sent, []string{"later"}) {
		t.Fatalf("capture gap lost later known content or claimed coverage: %v", sent)
	}
}
