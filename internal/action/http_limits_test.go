//go:build !race

package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
)

// Stream padding so the fixture does not allocate another whole maximum-size body.
type httpWhitespace struct{ remaining int }

func (r *httpWhitespace) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	for i := range p[:n] {
		p[i] = ' '
	}
	r.remaining -= n
	return n, nil
}

// The 256 MiB fixture exceeds the local 3 GiB check ceiling with race shadow memory; run it in the non-race suite.
func TestHTTPWireRouteCapsUseActualJSONBytes(t *testing.T) {
	for _, limit := range []int{httpControlBytes, httpReviewBytes, httpOutcomeBytes} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			for _, extra := range []int{0, 1} {
				body := io.MultiReader(strings.NewReader("{}"), &httpWhitespace{remaining: limit - 2 + extra})
				var value struct{}
				err := httpDecode(body, limit, &value)
				if extra == 0 && err != nil || extra == 1 && !errors.Is(err, errHTTPBodyLimit) {
					t.Fatalf("actual encoded boundary: extra=%d error=%v", extra, err)
				}
			}
		})
	}
	t.Run("assignment response", func(t *testing.T) {
		job := testJob(command.Comment)
		assignment := httpAssignment{Job: job, Expires: time.Now().Add(time.Hour).UnixNano()}
		body, err := json.Marshal(assignment)
		if err != nil {
			t.Fatal(err)
		}
		for _, extra := range []int{0, 1} {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/next" {
					t.Errorf("unexpected assignment route %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.Copy(w, io.MultiReader(bytes.NewReader(body), &httpWhitespace{remaining: httpJobBytes - len(body) + extra}))
			}))
			base, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			transport := httpExecutionClient{client: server.Client(), base: base}
			var got httpAssignment
			err = transport.post(context.Background(), "next", []byte(`{}`), httpJobBytes, &got, httpBinding{}, 0)
			server.Close()
			if extra == 0 && (err != nil || !reflect.DeepEqual(got, assignment)) || extra == 1 && !errors.Is(err, errHTTPBodyLimit) {
				t.Fatalf("assignment response boundary: extra=%d error=%v", extra, err)
			}
		}
	})
}
