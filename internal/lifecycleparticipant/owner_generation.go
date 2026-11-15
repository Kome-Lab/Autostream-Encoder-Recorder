package lifecycleparticipant

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// Owner generations are explicit nonsecret owner metadata. AdmissionModel is
// governed by its separate CAS journal; this snapshot must never infer current
// producer/vector state merely from a caller's cached object.
func (p *Participant) saveOwnerGeneration(s OwnerSnapshot) error {
	return p.write(fmt.Sprintf("owner-generation-%d.json", s.OwnerRevision), s)
}
func (p *Participant) checkOwnerGeneration() error {
	files, e := os.ReadDir(p.dir)
	if e != nil {
		return e
	}
	var latest OwnerSnapshot
	count := 0
	for _, f := range files {
		if !strings.HasPrefix(f.Name(), "owner-generation-") || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		count++
		if count > p.current.IngressProfile.Limits.MaxRefs {
			return errors.New("owner generation history limit exceeded")
		}
		b, e := os.ReadFile(filepath.Join(p.dir, f.Name()))
		if e != nil {
			return e
		}
		var s OwnerSnapshot
		if e = json.Unmarshal(b, &s); e != nil {
			return e
		}
		if s.OwnerRevision == 0 || f.Name() != fmt.Sprintf("owner-generation-%d.json", s.OwnerRevision) {
			return errors.New("invalid owner generation identity")
		}
		if s.OwnerRevision > latest.OwnerRevision {
			latest = s
		}
	}
	if latest.OwnerRevision == 0 {
		return errors.New("owner generation missing; held")
	}
	// Only the separately checked admission CAS state is excluded from this
	// equality. No generation, bound, profile or incarnation is defaulted.
	expected := clone(p.current)
	expected.AdmissionState = clone(latest.AdmissionState)
	if !reflect.DeepEqual(expected, latest) {
		return errors.New("owner generation changed; stale local owner snapshot")
	}
	return nil
}
