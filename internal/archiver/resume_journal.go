package archiver

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/restic/restic/internal/debug"
	"github.com/restic/restic/internal/repository/crypto"
	"github.com/restic/restic/internal/restic"
)

// resumeJournalMaxAge is the age after which a leftover journal is discarded.
const resumeJournalMaxAge = 30 * 24 * time.Hour

// ResumeJournal records which tree blob belongs to which snapshot path for
// every directory completed during a backup. If the backup is interrupted, the
// next run uses these trees as additional lookup candidates and thereby skips
// re-reading files in directories that were already finished.
//
// The journal is only ever a lookup hint: entries are verified against the
// index and the filesystem before use, so a stale, truncated or missing
// journal can never affect the content of a snapshot.
//
// A nil *ResumeJournal is valid and does nothing.
type ResumeJournal struct {
	mu       sync.Mutex
	path     string
	f        *os.File
	key      *crypto.Key
	trees    map[string]restic.ID
	lastSync time.Time
}

type resumeJournalEntry struct {
	Path string    `json:"path"`
	Tree restic.ID `json:"tree"`
}

// OpenResumeJournal loads the journal at path, if any, and opens it for
// appending. Entries are sealed with key, each on its own line. Lines that
// cannot be decoded (e.g. a torn last line after a crash) are ignored.
func OpenResumeJournal(path string, key *crypto.Key) (*ResumeJournal, error) {
	j := &ResumeJournal{path: path, key: key, trees: make(map[string]restic.ID), lastSync: time.Now()}

	if fi, err := os.Stat(path); err == nil && time.Since(fi.ModTime()) > resumeJournalMaxAge {
		debug.Log("discarding old resume journal %v", path)
		_ = os.Remove(path)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	j.f = f

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		buf, err := base64.StdEncoding.DecodeString(sc.Text())
		if err != nil || len(buf) < crypto.Extension {
			continue
		}
		nonce, ciphertext := buf[:key.NonceSize()], buf[key.NonceSize():]
		plaintext, err := key.Open(nil, nonce, ciphertext, nil)
		if err != nil {
			continue
		}
		var e resumeJournalEntry
		if json.Unmarshal(plaintext, &e) != nil {
			continue
		}
		j.trees[e.Path] = e.Tree
	}
	// a torn last line must not be merged with the first entry we append
	if _, err := f.WriteString("\n"); err != nil {
		_ = f.Close()
		return nil, err
	}
	debug.Log("resume journal %v: loaded %d entries", path, len(j.trees))
	return j, nil
}

// Len returns the number of directories in the journal.
func (j *ResumeJournal) Len() int {
	if j == nil {
		return 0
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.trees)
}

// Lookup returns the tree recorded for the directory at snPath.
func (j *ResumeJournal) Lookup(snPath string) (restic.ID, bool) {
	if j == nil {
		return restic.ID{}, false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	id, ok := j.trees[snPath]
	return id, ok
}

// Add records that the directory at snPath was saved as tree id. Errors are
// only logged, the journal is not essential.
func (j *ResumeJournal) Add(snPath string, id restic.ID) {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil || j.trees[snPath] == id {
		return
	}
	j.trees[snPath] = id

	plaintext, _ := json.Marshal(resumeJournalEntry{Path: snPath, Tree: id})
	nonce := crypto.NewRandomNonce()
	buf := j.key.Seal(nonce, nonce, plaintext, nil)
	if _, err := j.f.WriteString(base64.StdEncoding.EncodeToString(buf) + "\n"); err != nil {
		debug.Log("writing resume journal failed: %v", err)
		return
	}
	// bound the number of entries lost on power failure
	if time.Since(j.lastSync) > 30*time.Second {
		_ = j.f.Sync()
		j.lastSync = time.Now()
	}
}

// Close closes the journal. If remove is set, which is to be done once the
// backup completed, the journal file is deleted.
func (j *ResumeJournal) Close(remove bool) {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return
	}
	_ = j.f.Close()
	j.f = nil
	if remove {
		_ = os.Remove(j.path)
	}
}
