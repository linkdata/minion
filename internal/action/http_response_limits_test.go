package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
)

func TestHTTPControlResponseEncodedBoundary(t *testing.T) {
	for _, kind := range []string{"before", "after", "review", "status", "outcome", "next", "cancel"} {
		for _, negative := range []bool{false, true} {
			// /next's successful response has the separate assignment cap.
			if kind == "next" && !negative {
				continue
			}
			name := kind + "/success"
			if negative {
				name = kind + "/failure"
			}
			t.Run(name, func(t *testing.T) {
				job := testJob(command.Comment)
				binding := httpJobBinding(job)
				for _, extra := range []int{0, 1} {
					ack := httpAcknowledgement{httpBinding: binding, Kind: kind, Sequence: 1}
					if negative {
						ack.Failure = newHTTPFailure(context.Canceled)
					}
					var value any = ack
					if kind == "cancel" && !negative {
						value = httpCancellation{httpBinding: binding, Reason: "canceled", StopRemaining: httpControlTimeout}
					}
					body, err := json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path != "/"+kind {
							t.Errorf("unexpected control route %s", r.URL.Path)
						}
						w.Header().Set("Content-Type", "application/json")
						if negative {
							w.WriteHeader(http.StatusConflict)
						}
						_, _ = io.Copy(w, io.MultiReader(bytes.NewReader(body), strings.NewReader(strings.Repeat(" ", httpControlBytes-len(body)+extra))))
					}))
					base, err := url.Parse(server.URL)
					if err != nil {
						t.Fatal(err)
					}
					transport := &httpExecutionClient{client: server.Client(), base: base}
					if negative {
						_, err = transport.open(t.Context(), kind, []byte(`{}`), binding, 1)
					} else if kind == "cancel" {
						var response *http.Response
						response, err = transport.open(t.Context(), kind, []byte(`{}`), binding, 0)
						if err == nil {
							err = transport.watch(t.Context(), response, binding, func(time.Duration) {})
						}
					} else {
						client := httpClientCheckpoints{transport: transport, control: t.Context(), action: t.Context(), binding: binding}
						err = client.acknowledge(kind, []byte(`{}`), 1)
					}
					server.Close()
					wantCanceled := negative || kind == "cancel"
					if extra == 1 {
						if !errors.Is(err, errHTTPBodyLimit) {
							t.Fatalf("limit+1 response accepted: error=%v", err)
						}
					} else if wantCanceled && !errors.Is(err, context.Canceled) || !wantCanceled && err != nil {
						t.Fatalf("exact control response changed known outcome: error=%v", err)
					}
					unknown := extra == 1 && !(kind == "cancel" && negative)
					if errors.Is(err, monitor.ErrExecutionOutcomeUnknown) != unknown {
						t.Fatalf("response changed pre-admission versus held-watch uncertainty: error=%v wantUnknown=%t", err, unknown)
					}
				}
			})
		}
	}
}
