package auth

// Buff Cloud fork: keep each credential's last observed quota signals across
// restarts and auth file reloads, so room-based routing does not start blind.
// Upstream keeps them in memory only (cooldownFieldsOf drops them on purpose).

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// QuotaSignalFileName is the file, in the auth directory, holding the last
// observed quota signals per credential. It does not end in .json so the auth
// watcher and file store never read it as a credential.
const QuotaSignalFileName = "quota-signals.state"

// quotaSignalMaxAge drops observations older than any window they describe.
const quotaSignalMaxAge = 8 * 24 * time.Hour

const quotaSignalSaveDelay = 2 * time.Second

type quotaSignalRecord struct {
	ObservedAt time.Time         `json:"observed_at"`
	Signals    map[string]string `json:"signals"`
}

// QuotaSignalStore persists the last quota observation of each credential in one file.
type QuotaSignalStore struct {
	path string

	mu      sync.Mutex
	records map[string]quotaSignalRecord
	pending bool
	saveMu  sync.Mutex
}

// NewQuotaSignalStore loads the store at path; a missing or unreadable file starts empty.
func NewQuotaSignalStore(path string) *QuotaSignalStore {
	s := &QuotaSignalStore{path: path, records: make(map[string]quotaSignalRecord)}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Warnf("quota signals: read %s: %v", path, err)
		}
		return s
	}
	if errUnmarshal := json.Unmarshal(data, &s.records); errUnmarshal != nil {
		log.Warnf("quota signals: parse %s: %v", path, errUnmarshal)
		s.records = make(map[string]quotaSignalRecord)
	}
	return s
}

func (s *QuotaSignalStore) get(authID string) (QuotaState, bool) {
	if s == nil {
		return QuotaState{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[authID]
	if !ok {
		return QuotaState{}, false
	}
	return QuotaState{ObservedAt: record.ObservedAt, Signals: record.Signals}.Clone(), true
}

// record keeps the observation and schedules a save.
func (s *QuotaSignalStore) record(authID string, quota QuotaState) {
	if s == nil || authID == "" || quota.ObservedAt.IsZero() {
		return
	}
	quota = quota.Clone()
	s.mu.Lock()
	s.records[authID] = quotaSignalRecord{ObservedAt: quota.ObservedAt, Signals: quota.Signals}
	schedule := !s.pending
	s.pending = true
	s.mu.Unlock()
	if schedule {
		time.AfterFunc(quotaSignalSaveDelay, func() {
			if errSave := s.Save(); errSave != nil {
				log.Warnf("quota signals: save %s: %v", s.path, errSave)
			}
		})
	}
}

// forget drops a credential's saved observation and saves at once.
func (s *QuotaSignalStore) forget(authID string) {
	if s == nil || authID == "" {
		return
	}
	s.mu.Lock()
	_, had := s.records[authID]
	delete(s.records, authID)
	s.mu.Unlock()
	if had {
		if errSave := s.Save(); errSave != nil {
			log.Warnf("quota signals: save %s: %v", s.path, errSave)
		}
	}
}

// Save writes every observation younger than quotaSignalMaxAge, atomically and mode 600.
func (s *QuotaSignalStore) Save() error {
	if s == nil {
		return nil
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	s.pending = false
	cutoff := time.Now().Add(-quotaSignalMaxAge)
	for id, record := range s.records {
		if record.ObservedAt.Before(cutoff) {
			delete(s.records, id)
		}
	}
	data, err := json.MarshalIndent(s.records, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".quota-signals-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if errChmod := tmp.Chmod(0o600); errChmod != nil {
		_ = tmp.Close()
		return errChmod
	}
	if _, errWrite := tmp.Write(data); errWrite != nil {
		_ = tmp.Close()
		return errWrite
	}
	if errClose := tmp.Close(); errClose != nil {
		return errClose
	}
	return os.Rename(tmp.Name(), s.path)
}

// SetQuotaSignalStore makes the manager restore and record quota observations through store.
// Call it before Load so loaded credentials start with their last observation.
func (m *Manager) SetQuotaSignalStore(store *QuotaSignalStore) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.quotaSignals = store
	m.mu.Unlock()
}

// carryQuotaObservationLocked gives a credential being loaded, registered or
// replaced the newest of its own, its previous in-memory and its saved observation.
// Upstream starts a reloaded credential with none, and the linked auth files
// are rewritten (and so reloaded) during ordinary requests. Requires m.mu.
func (m *Manager) carryQuotaObservationLocked(auth, existing *Auth) {
	if auth == nil {
		return
	}
	if existing != nil {
		auth.Quota = mergeQuotaObservation(auth.Quota, existing.Quota)
	}
	if saved, ok := m.quotaSignals.get(auth.ID); ok {
		auth.Quota = mergeQuotaObservation(auth.Quota, saved)
	}
}
