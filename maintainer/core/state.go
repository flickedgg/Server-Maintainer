package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var ErrStateCorrupt = errors.New("state file was corrupt")

// State is the small amount of data the maintainer remembers between runs.
// The installed build ID is always re-read from the app manifest at startup;
// the copy here is for humans and for notifications.
type State struct {
	BuildID      string    `json:"build_id"`
	NewsGID      string    `json:"news_gid"`
	NewsDate     int64     `json:"news_date"`
	LastUpdateAt time.Time `json:"last_update_at"`
	ServerStarts int       `json:"server_starts"`
	Crashes      int       `json:"crashes"`
	// StoppedByAdmin is set by /stop on Discord and cleared by /restart. While
	// set, the server stays down, even across maintainer restarts.
	StoppedByAdmin bool `json:"stopped_by_admin"`
}

type StateStore struct {
	mu   sync.Mutex
	path string
	s    State
}

// OpenState loads the state file. A missing or empty file starts fresh. A corrupt
// file is moved aside and ErrStateCorrupt is returned along with a usable store,
// so a bad write can never keep the maintainer from starting.
func OpenState(path string) (*StateStore, error) {
	st := &StateStore{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading state: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return st, nil
	}
	if err := json.Unmarshal(data, &st.s); err != nil {
		st.s = State{}
		moved := path + ".corrupt"
		if rerr := os.Rename(path, moved); rerr != nil {
			return st, fmt.Errorf("%w (%v); could not move it aside: %v", ErrStateCorrupt, err, rerr)
		}
		return st, fmt.Errorf("%w (%v); moved to %s", ErrStateCorrupt, err, moved)
	}
	return st, nil
}

func (st *StateStore) Get() State {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.s
}

// Update applies fn and saves the result. The in-memory state keeps the change
// even if saving fails.
func (st *StateStore) Update(fn func(*State)) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	fn(&st.s)
	data, err := json.MarshalIndent(st.s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(st.path, append(data, '\n'))
}

// writeFileAtomic writes to a temp file in the same directory, syncs it and renames
// it over path, so readers see either the old file or the new one, never half.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	return finishAtomic(tmp, path, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

func finishAtomic(tmp *os.File, path string, write func(io.Writer) error) (err error) {
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if err = write(tmp); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
