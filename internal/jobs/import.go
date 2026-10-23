package jobs

// import-history (D-02, PD-23). The webhook-go fork keeps its deploy history in
// <state_dir>/jobs/<id>.json, the Job JSON plus the writing instance's "owner".
// The agent keeps that layout, so carrying the history across the cutover is a
// validated copy, not a conversion. The import reads the legacy directory and
// never writes to it: it is the rollback source.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxImportRecordBytes bounds one legacy record. The largest honest record is
// MaxEnvironments results of the output limit each; anything bigger is not one
// of the fork's records.
const maxImportRecordBytes = 64 << 20

// ImportReport counts what one ImportHistory did.
type ImportReport struct {
	Imported          int // finished records copied into the store
	SkippedExisting   int // finished records whose id the store already holds
	SkippedUnfinished int // queued or deploying records, which a stopped service left behind
	SkippedUnreadable int // *.json files that are not valid fork records
	// SkippedBeyondMax counts finished records older than the newest max_jobs
	// of the store and the import together: the store would delete them at
	// once, and a second run would then copy them again.
	SkippedBeyondMax int
}

// legacyRecord is the fork's record on disk and nothing else: decoding it
// strictly makes a file with any other key not a fork record.
type legacyRecord struct {
	Job
	Owner string `json:"owner"`
}

// ImportHistory copies every finished (deployed or failed) record in fromDir
// into dst, once. A record is read only if its file is a regular file named
// <32 hex>.json; it must decode strictly, carry the id in its name, and have
// known states and valid environment names. Ids dst already holds are kept as
// they are. The old owner is dropped (dst writes its own), nothing in fromDir
// is changed, and the normal retention of dst applies afterwards: records that
// retention would delete at once are not copied (SkippedBeyondMax), so running
// the import twice imports nothing the second time.
func ImportHistory(fromDir string, dst *FileStore) (ImportReport, error) {
	var rep ImportReport
	entries, err := os.ReadDir(fromDir)
	if err != nil {
		return rep, err
	}
	var candidates []Job
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() {
			continue // temporary files, the .corrupt directory, other directories
		}
		rec, ok := readLegacy(filepath.Join(fromDir, e.Name()), id, e)
		if !ok {
			rep.SkippedUnreadable++
			continue
		}
		switch {
		case !rec.State.Terminal():
			rep.SkippedUnfinished++
		case dst.has(rec.ID):
			rep.SkippedExisting++
		default:
			candidates = append(candidates, rec.Job)
		}
	}

	survivors, beyond := dst.withinRetention(candidates)
	rep.SkippedBeyondMax = beyond
	sort.SliceStable(survivors, func(a, b int) bool { return newer(survivors[b], survivors[a]) }) // oldest first
	for _, job := range survivors {
		if err := dst.Save(job); err != nil {
			return rep, err
		}
		rep.Imported++
	}
	return rep, nil
}

// readLegacy reads and validates one legacy record. ok is false for anything
// that is not a record the fork wrote.
func readLegacy(path, id string, e os.DirEntry) (legacyRecord, bool) {
	var rec legacyRecord
	if !jobIDRE.MatchString(id) || !e.Type().IsRegular() { // a symlink is never followed
		return rec, false
	}
	info, err := e.Info()
	if err != nil || info.Size() > maxImportRecordBytes {
		return rec, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return rec, false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil {
		return rec, false
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return rec, false
	}
	if err := validLegacy(rec, id); err != nil {
		return rec, false
	}
	return rec, true
}

func knownState(s State) bool {
	return s == StateQueued || s == StateDeploying || s == StateDeployed || s == StateFailed
}

// validLegacy checks what the store and the API rely on: the file name is the
// id, states are known, environment names could have been deployed.
func validLegacy(rec legacyRecord, id string) error {
	switch {
	case rec.ID != id:
		return fmt.Errorf("record names id %q", rec.ID)
	case rec.Kind != "":
		return errors.New("not a deploy record")
	case !knownState(rec.State):
		return fmt.Errorf("unknown state %q", rec.State)
	case len(rec.Results) == 0 || len(rec.Results) > MaxEnvironments:
		return fmt.Errorf("%d results", len(rec.Results))
	}
	for _, r := range rec.Results {
		if !ValidEnvironmentName(r.Environment) || !knownState(r.State) {
			return fmt.Errorf("result %q in state %q", r.Environment, r.State)
		}
	}
	return nil
}

// newer reports whether a sorts before b in the store's listing (newest first).
func newer(a, b Job) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.ID > b.ID
}

// has reports whether s holds a record file for id, whatever it contains.
func (s *FileStore) has(id string) bool {
	_, err := os.Lstat(s.path(id))
	return err == nil
}

// withinRetention splits candidates into those the store would keep once they
// are saved, next to the finished deploy records it holds now, and a count of
// those it would delete at once.
func (s *FileStore) withinRetention(candidates []Job) (keep []Job, beyond int) {
	if len(candidates) == 0 {
		return nil, 0
	}
	s.mu.Lock()
	recs, err := s.allStoredLocked()
	s.mu.Unlock()
	if err != nil { // cannot rank them: copy none rather than copy blindly
		return nil, 0
	}
	type ranked struct {
		job       Job
		candidate bool
	}
	var all []ranked
	for _, r := range recs {
		if !r.Refused && r.Kind == "" && r.State.Terminal() {
			all = append(all, ranked{job: r.Job})
		}
	}
	for _, c := range candidates {
		all = append(all, ranked{job: c, candidate: true})
	}
	sort.SliceStable(all, func(a, b int) bool { return newer(all[a].job, all[b].job) })
	for i, r := range all {
		switch {
		case !r.candidate:
		case i < s.maxJobs:
			keep = append(keep, r.job)
		default:
			beyond++
		}
	}
	return keep, beyond
}
