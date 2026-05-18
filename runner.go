package main

import (
	"context"
	"errors"
	"fmt"

	run "cloud.google.com/go/run/apiv2"
	"cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// JobOverrides is the per-execution payload forwarded to the target job.
// Args == nil keeps the job's baked-in args; a non-nil slice replaces them.
type JobOverrides struct {
	Env  map[string]string
	Args []string
}

type JobRunner interface {
	Run(ctx context.Context, project, region, jobName string, o JobOverrides) error
}

// PermanentError signals that retrying the same RunJob call will not succeed.
// The handler converts this into a 2xx ack so Pub/Sub stops redelivering.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return "permanent: " + e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// adminRunner is the production JobRunner backed by Cloud Run Admin v2.
type adminRunner struct {
	client *run.JobsClient
}

func NewAdminRunner(ctx context.Context) (*adminRunner, error) {
	c, err := run.NewJobsClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("new jobs client: %w", err)
	}
	return &adminRunner{client: c}, nil
}

func (r *adminRunner) Close() error { return r.client.Close() }

func (r *adminRunner) Run(ctx context.Context, project, region, jobName string, o JobOverrides) error {
	name := fmt.Sprintf("projects/%s/locations/%s/jobs/%s", project, region, jobName)

	envVars := make([]*runpb.EnvVar, 0, len(o.Env))
	for k, v := range o.Env {
		envVars = append(envVars, &runpb.EnvVar{
			Name:   k,
			Values: &runpb.EnvVar_Value{Value: v},
		})
	}

	container := &runpb.RunJobRequest_Overrides_ContainerOverride{
		Env: envVars,
	}
	if o.Args != nil {
		container.Args = o.Args
		if len(o.Args) == 0 {
			container.ClearArgs = true
		}
	}

	req := &runpb.RunJobRequest{
		Name: name,
		Overrides: &runpb.RunJobRequest_Overrides{
			ContainerOverrides: []*runpb.RunJobRequest_Overrides_ContainerOverride{container},
		},
	}

	if _, err := r.client.RunJob(ctx, req); err != nil {
		if isPermanent(err) {
			return &PermanentError{Err: err}
		}
		return err
	}
	return nil
}

func isPermanent(err error) bool {
	if err == nil {
		return false
	}
	if s, ok := status.FromError(err); ok {
		switch s.Code() {
		case codes.NotFound,
			codes.PermissionDenied,
			codes.InvalidArgument,
			codes.FailedPrecondition,
			codes.Unauthenticated:
			return true
		}
	}
	var perm *PermanentError
	return errors.As(err, &perm)
}
