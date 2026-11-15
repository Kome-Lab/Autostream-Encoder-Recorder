package lifecycleparticipant

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	lifecycle "github.com/Kome-Lab/Autostream-Contracts/v3lifecycle"
)

// The isolated owner guard is process-shared and fail-closed after a crashed
// holder. A leftover guard is never automatically deleted or treated as absent.
func (p *Participant) acquireOwnerGuard() (func(), error) {
	path := filepath.Join(p.dir, "active-owner.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	return func() { _ = file.Close(); _ = os.Remove(path) }, nil
}
func (p *Participant) checkOwnerState() error {
	b, e := os.ReadFile(filepath.Join(p.dir, "owner.genesis.json"))
	if e != nil {
		return e
	}
	var latest lifecycle.AdmissionModel
	if e = json.Unmarshal(b, &latest); e != nil {
		return e
	}
	files, e := os.ReadDir(p.dir)
	if e != nil {
		return e
	}
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".start.json") {
			continue
		}
		b, e = os.ReadFile(filepath.Join(p.dir, f.Name()))
		if e != nil {
			return e
		}
		var entry journalEntry
		if e = json.Unmarshal(b, &entry); e != nil {
			return e
		}
		if entry.AdmissionState.Revision > latest.Revision {
			latest = entry.AdmissionState
		}
	}
	if b, e = os.ReadFile(filepath.Join(p.dir, "admission.closed.json")); e == nil {
		var closed closedJournal
		if e = json.Unmarshal(b, &closed); e != nil {
			return e
		}
		latest = closed.State
	} else if !os.IsNotExist(e) {
		return e
	}
	if !reflect.DeepEqual(latest, p.admission) {
		return errors.New("owner state changed in another process; inspect current state")
	}
	return p.checkOwnerGeneration()
}
func (p *Participant) readAttempt(k string) (journalEntry, error) {
	b, e := os.ReadFile(filepath.Join(p.dir, k+".start.json"))
	if e != nil {
		return journalEntry{}, e
	}
	var entry journalEntry
	if e = json.Unmarshal(b, &entry); e != nil {
		return journalEntry{}, e
	}
	for rev := 1; ; rev++ {
		b, e = os.ReadFile(filepath.Join(p.dir, fmt.Sprintf("%s.result-%d.json", k, rev)))
		if os.IsNotExist(e) {
			break
		}
		if e != nil {
			return journalEntry{}, e
		}
		var next journalEntry
		if e = json.Unmarshal(b, &next); e != nil {
			return journalEntry{}, e
		}
		if !reflect.DeepEqual(next.Request, entry.Request) {
			return journalEntry{}, errors.New("immutable attempt changed")
		}
		entry = next
	}
	return entry, nil
}
