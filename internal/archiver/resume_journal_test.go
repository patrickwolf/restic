package archiver

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/restic/restic/internal/data"
	"github.com/restic/restic/internal/fs"
	"github.com/restic/restic/internal/repository"
	"github.com/restic/restic/internal/repository/crypto"
	"github.com/restic/restic/internal/repository/index"
	"github.com/restic/restic/internal/restic"
	rtest "github.com/restic/restic/internal/test"
)

func TestResumeJournal(t *testing.T) {
	path := filepath.Join(rtest.TempDir(t), "resume", "journal")
	key := crypto.NewRandomKey()
	id1, id2 := restic.NewRandomID(), restic.NewRandomID()

	j, err := OpenResumeJournal(path, key)
	rtest.OK(t, err)
	j.Add("/secret/a", id1)
	j.Add("/secret/b", id1)
	j.Add("/secret/b", id2)
	j.Close(false)

	buf, err := os.ReadFile(path)
	rtest.OK(t, err)
	rtest.Assert(t, !bytes.Contains(buf, []byte("/secret/a")) && !bytes.Contains(buf, []byte(id1.String())), "journal contains plaintext")
	// simulate a torn write
	rtest.OK(t, os.WriteFile(path, append(buf, "dG9ybg"...), 0600))

	j, err = OpenResumeJournal(path, key)
	rtest.OK(t, err)
	rtest.Equals(t, 2, j.Len())
	id, ok := j.Lookup("/secret/a")
	rtest.Assert(t, ok && id == id1, "wrong entry for a")
	id, ok = j.Lookup("/secret/b")
	rtest.Assert(t, ok && id == id2, "wrong entry for b")
	// must not be glued to the torn line
	j.Add("/c", id1)
	j.Close(false)

	// entries of a different key are ignored
	j, err = OpenResumeJournal(path, crypto.NewRandomKey())
	rtest.OK(t, err)
	rtest.Equals(t, 0, j.Len())
	j.Close(false)

	j, err = OpenResumeJournal(path, key)
	rtest.OK(t, err)
	rtest.Equals(t, 3, j.Len())
	j.Close(true)
	_, err = os.Stat(path)
	rtest.Assert(t, os.IsNotExist(err), "journal not removed: %v", err)

	// nil journal is a no-op
	var nj *ResumeJournal
	nj.Add("/secret/a", id1)
	_, ok = nj.Lookup("/secret/a")
	rtest.Assert(t, !ok, "nil journal returned entry")
	nj.Close(true)
}

// resumeTestSnapshot runs a backup of targets in the current directory and
// returns which files were read.
func resumeTestSnapshot(t *testing.T, repo *repository.Repository, j *ResumeJournal, targets []string, parent *data.Snapshot) (*data.Snapshot, *Summary, map[string]int) {
	testFS := &MockFS{FS: fs.NewLocal(), bytesRead: make(map[string]int)}
	arch := New(repo, testFS, Options{})
	arch.Journal = j
	arch.Error = func(item string, err error) error {
		t.Errorf("unexpected error for %v: %v", item, err)
		return err
	}
	_, id, summary, err := arch.Snapshot(context.TODO(), targets, SnapshotOptions{Time: time.Now(), ParentSnapshot: parent})
	rtest.OK(t, err)
	sn, err := data.LoadSnapshot(context.TODO(), repo, id)
	rtest.OK(t, err)
	return sn, summary, testFS.bytesRead
}

// failFS blocks opening the file at path until ready returns true and then fails.
type failFS struct {
	fs.FS
	path  string
	ready func() bool
}

var errTestInterrupted = errors.New("interrupted")

func (f *failFS) OpenFile(name string, flag int, metadataOnly bool) (fs.File, error) {
	if name == f.path {
		for start := time.Now(); !f.ready() && time.Since(start) < 10*time.Second; {
			time.Sleep(time.Millisecond)
		}
		return nil, errTestInterrupted
	}
	return f.FS.OpenFile(name, flag, metadataOnly)
}

func TestArchiverResumeInterrupted(t *testing.T) {
	// flush tree packs and index right away instead of every few minutes
	origFull, origInterval := index.Full, treePackFlushInterval
	index.Full = func(*index.Index) bool { return true }
	treePackFlushInterval = 0
	defer func() { index.Full, treePackFlushInterval = origFull, origInterval }()

	repo, be := repository.TestRepositoryWithBackend(t, nil, 0, repository.Options{})
	tempdir := rtest.TempDir(t)
	TestCreateFiles(t, tempdir, TestDir{
		"done":    TestDir{"a": TestFile{Content: "aaa"}, "sub": TestDir{"b": TestFile{Content: "bbb"}}},
		"pending": TestDir{"c": TestFile{Content: "ccc"}},
	})
	defer rtest.Chdir(t, tempdir)()
	targets := []string{"done", "pending"}
	journalPath := filepath.Join(rtest.TempDir(t), "journal")

	// Data packs are only uploaded when full, which the tiny test files never
	// achieve. Store the file contents beforehand, the snapshot is only used
	// as parent at the very end.
	parent, _, _ := resumeTestSnapshot(t, repo, nil, targets, nil)

	// first run is interrupted after "done" has been completed
	j, err := OpenResumeJournal(journalPath, repo.Key())
	rtest.OK(t, err)
	arch := New(repo, &failFS{FS: fs.NewLocal(), path: filepath.Join("pending", "c"), ready: func() bool {
		_, ok := j.Lookup("/done")
		return ok
	}}, Options{})
	arch.Journal = j
	_, _, _, err = arch.Snapshot(context.TODO(), targets, SnapshotOptions{Time: time.Now()})
	rtest.Assert(t, errors.Is(err, errTestInterrupted), "unexpected error %v", err)
	j.Close(false)

	// only what has been written to the repository is known to the next run
	repo = repository.TestOpenBackend(t, be)
	rtest.OK(t, repo.LoadIndex(context.TODO(), restic.NoopTerminalCounterFactory))
	j, err = OpenResumeJournal(journalPath, repo.Key())
	rtest.OK(t, err)
	// an entry whose tree never made it into the repository is skipped silently
	j.Add("/pending", restic.NewRandomID())

	// without parent: completed directories are not read again
	_, summary, read := resumeTestSnapshot(t, repo, j, targets, nil)
	rtest.Assert(t, len(read) == 1, "wrong files read: %v", read)
	rtest.Assert(t, read[filepath.Join("pending", "c")] > 0, "pending/c not read: %v", read)
	// statistics are relative to the parent snapshot, of which there is none
	rtest.Equals(t, ChangeStats{New: 3}, summary.Files)

	// journal and parent snapshot together
	_, summary, read = resumeTestSnapshot(t, repo, j, targets, parent)
	rtest.Assert(t, len(read) == 0, "files were read: %v", read)
	rtest.Equals(t, ChangeStats{Unchanged: 3}, summary.Files)

	// a file that changed after the interrupted run is read despite the journal
	rtest.OK(t, os.WriteFile(filepath.Join("done", "a"), []byte("changed"), 0600))
	_, _, read = resumeTestSnapshot(t, repo, j, targets, nil)
	rtest.Assert(t, read[filepath.Join("done", "a")] > 0 && read[filepath.Join("done", "sub", "b")] == 0, "wrong files read: %v", read)
	j.Close(true)

	// the next run has neither journal nor parent
	_, _, read = resumeTestSnapshot(t, repo, nil, targets, nil)
	rtest.Assert(t, len(read) == 3, "wrong files read: %v", read)
}
