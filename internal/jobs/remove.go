package jobs

// Environment removal (D-09). A removal is a Job of Kind KindRemove with one
// result, run by Runner.Run under the same deploy lock as a deploy, so it can
// never race one. The environment directory is renamed to a name Puppet Server
// does not read as an environment, and only then deleted, so the live name
// disappears in one atomic step and a crash half way leaves a hidden leftover
// that SweepRemovals deletes at the next start.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// sweepLockWait bounds how long SweepRemovals waits for the deploy lock at
// start: another agent process on the same state directory may be deploying.
const sweepLockWait = 10 * time.Second

// removingPrefix starts the name an environment is renamed to while it is
// deleted: <basedir>/.removing-<env>-<jobid>. A leading dot is not a valid
// environment name, so Puppet Server never compiles from it (RESEARCH A5).
const removingPrefix = ".removing-"

// NewRemoval validates env, checks that <basedir>/<env> exists, and records a
// queued removal. The name rule admits no separator or dot, so the path cannot
// leave basedir. A symlink or non-directory at the path is not refused here:
// the job fails, naming why, so the caller sees it in the record.
func (r *Runner) NewRemoval(env string) (Job, error) {
	if !ValidEnvironmentName(env) {
		return Job{}, fmt.Errorf("%w: invalid environment name %q", ErrInvalidRequest, env)
	}
	if _, err := os.Lstat(filepath.Join(r.opts.Basedir, env)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Job{}, ErrEnvironmentNotFound
		}
		return Job{}, fmt.Errorf("checking environment %s: %w", env, err)
	}
	job := Job{
		ID:        newJobID(),
		Engine:    r.opts.Engine.Name,
		Kind:      KindRemove,
		State:     StateQueued,
		CreatedAt: time.Now().UTC(),
		Results:   []EnvResult{{Environment: env, State: StateQueued}},
	}
	if err := r.opts.Store.Save(job); err != nil {
		return Job{}, err
	}
	return job, nil
}

// removeOne removes the environment named by res: look at the path without
// following links, rename it aside, delete the renamed path. It runs under the
// deploy lock. Nothing is ever removed except <basedir>/<env> itself, whose
// name was validated, and the directory it was renamed to.
func (r *Runner) removeOne(ctx context.Context, res EnvResult, jobID string) EnvResult {
	res.StartedAt = time.Now().UTC()
	finish := func(state State, code int, msg string) EnvResult {
		res.State, res.ExitCode, res.Error = state, code, msg
		res.FinishedAt = time.Now().UTC()
		res.DurationMS = res.FinishedAt.Sub(res.StartedAt).Milliseconds()
		return res
	}
	// Defence in depth: the API validated these already.
	if !ValidEnvironmentName(res.Environment) || !jobIDRE.MatchString(jobID) {
		return finish(StateFailed, -1, "invalid environment request")
	}
	if err := ctx.Err(); err != nil {
		return finish(StateFailed, -1, "not run: "+ErrShutdown.Error())
	}
	path := filepath.Join(r.opts.Basedir, res.Environment)
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return finish(StateFailed, -1, "environment not found")
	case err != nil:
		return finish(StateFailed, -1, "could not look at the environment: "+err.Error())
	case !info.IsDir(): // Lstat: a symlink is not a directory here
		return finish(StateFailed, -1, "refusing to remove: not a directory")
	}
	aside := filepath.Join(r.opts.Basedir, removingPrefix+res.Environment+"-"+jobID)
	r.log.Info("removing environment", "env", res.Environment, "job_id", jobID)
	if err := os.Rename(path, aside); err != nil {
		return finish(StateFailed, -1, "could not move the environment aside: "+err.Error())
	}
	if err := os.RemoveAll(aside); err != nil {
		// The live name is already gone; the leftover is swept at the next start.
		return finish(StateFailed, -1, "environment moved aside but not fully deleted: "+err.Error())
	}
	return finish(StateDeployed, 0, "")
}

// SweepRemovals deletes the <basedir>/.removing-* directories a crash between
// the rename and the delete left behind, and returns how many it deleted. Only
// real directories are touched: it looks at each entry without following links,
// so a symlink or a file with such a name is logged and left alone. It takes
// the deploy lock, so it cannot run under another process's removal; if the
// lock stays busy for sweepLockWait it gives up with an error and the leftovers
// wait for the next start. The server calls it once at start, after the store
// is reconciled and before requests are accepted.
func (r *Runner) SweepRemovals() (int, error) {
	ctx, cancel := context.WithTimeout(r.ctx, sweepLockWait)
	defer cancel()
	release, err := r.lock.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("sweeping removals: waiting for the deploy lock: %w", err)
	}
	defer release()
	entries, err := os.ReadDir(r.opts.Basedir)
	if err != nil {
		return 0, fmt.Errorf("sweeping removals: %w", err)
	}
	n := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), removingPrefix) {
			continue
		}
		path := filepath.Join(r.opts.Basedir, e.Name())
		if !e.Type().IsDir() { // DirEntry types come from Lstat: a symlink is not a directory
			r.log.Warn("left a non-directory named like a removal leftover alone", "path", path)
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			r.log.Error("could not delete a removal leftover", "path", path, "error", err.Error())
			continue
		}
		r.log.Info("deleted a removal leftover", "path", path)
		n++
	}
	return n, nil
}
