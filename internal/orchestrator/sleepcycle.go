package orchestrator

import (
	"log"
	"net/http"
)

// handleTriggerSleepCycle triggers the Phase-2 Sleep Cycle for a goal: it
// validates the goal exists, launches the named job (an infra-level invocation,
// not a direct code call into the worker), audits the trigger, and returns 202.
func (s *Server) handleTriggerSleepCycle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	goal, ok := s.lookupGoal(ctx, w, r.PathValue("id"))
	if !ok {
		return
	}

	if err := s.jobs.Launch(ctx, s.sleepCycleJobName, map[string]string{
		"optimization_function_id": goal.OptimizationFunctionID,
	}); err != nil {
		log.Printf("orchestrator: launch sleep-cycle job: %v", err)
		writeErr(w, http.StatusInternalServerError, "failed to launch sleep cycle")
		return
	}

	if err := s.recordAudit(ctx, "sleep_cycle_trigger", "job", map[string]any{
		"optimization_function_id": goal.OptimizationFunctionID,
		"job_name":                 s.sleepCycleJobName,
	}); err != nil {
		log.Printf("orchestrator: append audit: %v", err)
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"optimization_function_id": goal.OptimizationFunctionID})
}
