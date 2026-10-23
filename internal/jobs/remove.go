package jobs

import (
	"context"
	"errors"
)

// NewRemoval is a stub until the GREEN commit.
func (r *Runner) NewRemoval(env string) (Job, error) {
	return Job{}, errors.New("not implemented")
}

// removeOne is a stub until the GREEN commit.
func (r *Runner) removeOne(ctx context.Context, res EnvResult, jobID string) EnvResult {
	return res
}
