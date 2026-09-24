package main

import (
	"context"
	"testing"
	"time"

	"github.com/hibiken/asynq"
)

// Regression test for the "every deployment sits idle for its full timeout
// before a worker touches it" bug: EnqueueDeploymentWithConfig was passing
// config.Timeout to asynq.ProcessAt (delay the task's eligibility) instead of
// asynq.Timeout (cap how long the worker may spend running it). That put
// every enqueued task into asynq's "scheduled" set with an eligibility time
// far in the future, so the single worker never saw it until the timeout had
// already elapsed on its own.
//
// This exercises the real enqueue path against a local Redis and asserts the
// task lands in "pending" (immediately workable) rather than "scheduled".
func TestEnqueueDeploymentDoesNotDelayEligibility(t *testing.T) {
	manager, err := NewDeploymentJobManager(JobConfig{
		Timeout:    2 * time.Second, // small on purpose; magnitude doesn't matter, only ProcessAt-vs-Timeout does
		MaxRetries: 0,
		Priority:   PriorityNormal,
	})
	if err != nil {
		t.Skipf("skipping: could not reach local redis for asynq: %v", err)
	}
	defer manager.Close()

	ctx := context.Background()
	jobID, err := manager.EnqueueDeploymentWithConfig(ctx, "test-user", "test-deployment", true, manager.config)
	if err != nil {
		t.Fatalf("EnqueueDeploymentWithConfig: %v", err)
	}

	// Clean up everything this test wrote, regardless of outcome.
	defer func() {
		inspector := asynq.NewInspector(manager.redisOpt)
		defer inspector.Close()
		_ = inspector.DeleteTask(deploymentJobQueueName, jobID)
		_ = manager.redisClient.Del(ctx, manager.statusKey(jobID)).Err()
	}()

	inspector := asynq.NewInspector(manager.redisOpt)
	defer inspector.Close()

	info, err := inspector.GetTaskInfo(deploymentJobQueueName, jobID)
	if err != nil {
		t.Fatalf("GetTaskInfo: %v", err)
	}

	if info.State != asynq.TaskStatePending {
		t.Fatalf("task state = %v, want %v (task should be immediately workable, not scheduled for later — got NextProcessAt=%v)",
			info.State, asynq.TaskStatePending, info.NextProcessAt)
	}
}
