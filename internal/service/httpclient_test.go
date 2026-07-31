package service

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDrainAndClose pins both edges of the limit at once, because the two cases
// only mean something relative to each other: a body under it keeps the
// connection, a body over it does not. Sizing them against drainLimit rather than
// at round numbers is deliberate -- bodies at a fixed 8KiB/1MiB would leave every
// value in between passing, and a silently shrunk limit stops draining the
// gateway error pages this exists for.
//
// Reuse is observable only across two requests -- see DrainAndClose for why a
// body must reach EOF first.
func TestDrainAndClose(t *testing.T) {
	if drainLimit != 64<<10 {
		t.Fatalf("drainLimit = %d, want 64 KiB -- large enough for a gateway error page", drainLimit)
	}

	// Sized well clear of the limit on both sides. The exact byte at which reuse
	// flips depends on io.Discard's buffer alignment, which is net/http's business
	// rather than this helper's contract.
	cases := []struct {
		name         string
		bodySize     int
		wantSameConn bool
	}{
		{"a body within the limit is drained", drainLimit / 8, true},
		{"a body past the limit abandons the connection", drainLimit * 8, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var addrs []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				addrs = append(addrs, r.RemoteAddr)
				// Not JSON: this is the shape of the gateway error page a caller
				// abandons after its decode fails.
				w.WriteHeader(http.StatusBadGateway)
				w.Write(bytes.Repeat([]byte("x"), tc.bodySize))
			}))
			defer srv.Close()

			client := srv.Client()
			for range 2 {
				resp, err := client.Get(srv.URL)
				if err != nil {
					t.Fatalf("get: %v", err)
				}
				DrainAndClose(resp)
			}

			if len(addrs) != 2 {
				t.Fatalf("server saw %d request(s), want 2", len(addrs))
			}
			if sameConn := addrs[0] == addrs[1]; sameConn != tc.wantSameConn {
				t.Fatalf("connections = %v (reused = %v), want reused = %v", addrs, sameConn, tc.wantSameConn)
			}
		})
	}
}
