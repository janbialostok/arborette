// Package service holds small helpers shared by the cmd/<service> mains. The
// service binaries wire config and connections, then block until a shutdown
// signal so the container stays up in compose.
package service

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

// WaitForShutdown blocks until SIGINT or SIGTERM, logging readiness and the
// received signal under the service name.
func WaitForShutdown(name string) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	log.Printf("%s: ready, blocking until shutdown signal", name)
	received := <-sig
	log.Printf("%s: received %s, shutting down", name, received)
}
