//go:build !race

package action

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
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

// The 256 MiB fixture exceeds the 3 GiB check ceiling with race shadow memory; run it in the non-race suite.
func TestHTTPWireRouteCapsUseActualJSONBytes(t *testing.T) {
	for _, limit := range []int{httpControlBytes, httpReviewBytes, httpOutcomeBytes, httpJobBytes} {
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
}
