//go:build temporal

package temporaldriver

import (
	"context"

	sdkclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// DialSDK connects with the SDK's concrete client type (the worker host
// needs it; the API side only needs the Driver's narrowed interface).
func DialSDK(ctx context.Context, cfg Config) (sdkclient.Client, error) {
	return dialSDK(ctx, cfg.withDefaults())
}

// RunWorker hosts the driver's workflows and activities on cfg.TaskQueue
// until ctx is done. Deploy it in the worker process with a Store wired for
// audit parity (NewPostgresStore) and a workflows.Services implementation for
// the message steps — the same shape cmd/worker feeds the default engine.
//
// Shutdown drains in-flight tasks via worker.Stop; Temporal redelivers
// anything interrupted, so kill -9 mid-step loses nothing.
func RunWorker(ctx context.Context, cl sdkclient.Client, cfg Config, acts *Activities) error {
	cfg = cfg.withDefaults()
	w := worker.New(cl, cfg.TaskQueue, worker.Options{})
	RegisterWorkflows(w, acts)
	RegisterActivities(w, acts)
	if err := w.Start(); err != nil {
		return apperrors.Internal("temporal.worker_start_failed", "temporal worker failed to start").WithCause(err)
	}
	<-ctx.Done()
	w.Stop()
	return nil
}
