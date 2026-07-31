package service

import (
	"io"
	"net/http"
)

// drainLimit caps how much of an unread body is consumed. It bounds the work a
// hostile or broken peer can impose: past this the connection is worth less than
// the read.
const drainLimit = 64 << 10

// DrainAndClose releases a response body, first consuming what the caller left
// unread. net/http returns a connection to the idle pool only once its body has
// been read to EOF and closed, so a body abandoned early -- a gateway's HTML
// error page, a half-decoded response -- otherwise costs a fresh TCP connection
// on every retry, exactly when the peer is already failing.
//
// Callers defer it in place of resp.Body.Close(). The copy's error is dropped
// because the outcome is already decided by then: a failed drain costs connection
// reuse and nothing else.
//
// The reader is given one byte past the limit so a body sized exactly at it is
// still reused. io.LimitReader stops at its cap without issuing the further read
// that lets the connection observe EOF, so a cap of exactly drainLimit can drain
// a whole body and discard the connection anyway.
func DrainAndClose(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit+1))
	resp.Body.Close()
}
