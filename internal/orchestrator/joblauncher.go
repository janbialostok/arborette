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
//
// Argument contract the production launcher must honor: the worker binary takes
// the goal as `-goal <optimization_function_id>` from the args map key of that
// name, or SLEEPCYCLE_GOAL_ID as an environment fallback.
type StubLauncher struct{}

// Launch logs the invocation the production launcher would dispatch.
func (StubLauncher) Launch(_ context.Context, jobName string, args map[string]string) error {
	log.Printf("orchestrator: sleep-cycle job %q invoked with args %v", jobName, args)
	return nil
}
