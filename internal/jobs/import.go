package jobs

// ImportReport is a stub until the GREEN commit.
type ImportReport struct {
	Imported          int
	SkippedExisting   int
	SkippedUnfinished int
	SkippedUnreadable int
	SkippedBeyondMax  int
}

// ImportHistory is a stub until the GREEN commit.
func ImportHistory(fromDir string, dst *FileStore) (ImportReport, error) {
	return ImportReport{}, nil
}
