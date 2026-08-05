// Package service holds the HTTP plumbing every service shares and none of them
// owns: serving and graceful shutdown for the cmd/<service> mains, the bearer
// guard, the JSON response writers the handlers answer through, and the response
// cleanup the clients defer. A helper belongs here once a second service would
// otherwise copy it.
package service

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// shutdownTimeout bounds how long a graceful Shutdown waits for in-flight
// requests to drain before the context is cancelled.
const shutdownTimeout = 15 * time.Second

// readHeaderTimeout bounds how long the server waits for a request's headers,
// closing the slow-client (Slowloris) vector on connections that trickle bytes.
const readHeaderTimeout = 10 * time.Second

// RunHTTPServer starts an http.Server on addr and blocks until either the server
// fails to start or a SIGINT/SIGTERM arrives, then drains in-flight requests. It
// selects over both the ListenAndServe error and the signal so a startup failure
// (port already bound, bad addr) returns immediately instead of leaving the
// process alive with no listener waiting on a signal that ends it.
func RunHTTPServer(name, addr string, handler http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: readHeaderTimeout}

	serveErr := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	log.Printf("%s: listening on %s, blocking until shutdown signal", name, addr)

	select {
	case err := <-serveErr:
		return err
	case received := <-sig:
		log.Printf("%s: received %s, shutting down", name, received)
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return srv.Shutdown(ctx)
	}
}
