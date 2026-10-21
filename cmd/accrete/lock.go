package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// stateLock is a state file's lock: <state>.lock, created exclusively, so
// two applies never write one state. It holds who took it, so a lock left by
// a crash can be told from one that is held. See openspec accretion-apply,
// D7.
type stateLock struct {
	Host    string    `json:"host"`
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
	path    string
}

// lockState takes the lock on a state file. A lock left on this host by a
// process that is no longer running is taken over, and note says so; any
// other is refused with its path, and never removed.
func lockState(statePath string, note func(string)) (*stateLock, error) {
	path := statePath + ".lock"
	host, _ := os.Hostname()
	l := &stateLock{Host: host, PID: os.Getpid(), Started: time.Now().UTC(), path: path}
	raw, err := json.Marshal(l)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, werr := f.Write(raw)
			cerr := f.Close()
			if werr != nil || cerr != nil {
				os.Remove(path)
				return nil, errors.Join(werr, cerr)
			}
			return l, nil
		}
		if !errors.Is(err, fs.ErrExist) || attempt > 0 {
			return nil, err
		}
		var held stateLock
		old, rerr := os.ReadFile(path)
		if rerr != nil || json.Unmarshal(old, &held) != nil || held.PID <= 0 {
			return nil, fmt.Errorf("%s exists and does not say who holds it; another accrete may be running. Remove it once you are sure none is", path)
		}
		if held.Host != host || processAlive(held.PID) {
			return nil, fmt.Errorf("%s is held by pid %d on %s since %s; another accrete is running on this state. Remove the file only once you are sure it is not",
				path, held.PID, held.Host, held.Started.Format(time.RFC3339))
		}
		note(fmt.Sprintf("took over %s, left by pid %d on this host, which is no longer running", path, held.PID))
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%s: could not take the lock", path)
}

func (l *stateLock) release() { os.Remove(l.path) }
