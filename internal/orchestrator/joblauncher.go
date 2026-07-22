package orchestrator

import (
	"context"
	"log"
)

// JobLauncher invokes a named ephemeral job (the Sleep-Cycle Worker) as an
// infra-level invocation, not a direct code call into the worker. The contract
// is the job/container name; the launcher owns how it is dispatched.
type JobLauncher interface {
	Launch(ctx context.Context, jobName string, args map[string]string) error
}

// StubLauncher records the named-job invocation and returns success. It does not
// shell out to docker: the compose orchestrator block mounts no Docker socket,
// so a real docker invocation would fail at runtime. The production seam (AWS
// Batch SubmitJob) replaces this behind the interface.
type StubLauncher struct{}

// Launch logs the invocation the production launcher would dispatch.
func (StubLauncher) Launch(_ context.Context, jobName string, args map[string]string) error {
	log.Printf("orchestrator: sleep-cycle job %q invoked with args %v", jobName, args)
	return nil
}
