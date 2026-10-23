package websession

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	DefaultTTL  = 12 * time.Hour
	RememberTTL = 30 * 24 * time.Hour
	MaxSessions = 32
)

// Session represents an active session record in memory.
type Session struct {
	Expires    time.Time
	CSRF       string
	RememberMe bool
	TokenHash  [32]byte
}

// PersistedRecord is stored on disk for remember_me sessions only.
type PersistedRecord struct {
	TokenHashHex string `json:"token_hash"`
	CSRF         string `json:"csrf"`
	ExpiresAt    string `json:"expires_at"`
}

// PersistedStore is the JSON format of the persistence file.
type PersistedStore struct {
	Version     int               `json:"version"`
	Fingerprint string            `json:"fingerprint"`
	Sessions    []PersistedRecord `json:"sessions"`
}

// Manager manages memory sessions and persistence for remember_me sessions.
type Manager struct {
	mu          sync.Mutex
	filePath    string
	fingerprint string
	sessions    map[[32]byte]Session
	nowFunc     func() time.Time
}

// Option allows configuring the session manager (e.g. for testing clock).
type Option func(*Manager)

func WithNowFunc(fn func() time.Time) Option {
	return func(m *Manager) {
		m.nowFunc = fn
	}
}

// New creates a new session manager.
// If filePath is empty, persistence is disabled.
// If persistence file exists and is invalid/corrupted, it returns an error (fail-closed).
func New(filePath string, fingerprint string, opts ...Option) (*Manager, error) {
	m := &Manager{
		filePath:    filePath,
		fingerprint: fingerprint,
		sessions:    make(map[[32]byte]Session),
		nowFunc:     time.Now,
	}
	for _, opt := range opts {
		opt(m)
	}

	if filePath != "" {
		if err := m.load(); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (m *Manager) now() time.Time {
	if m.nowFunc != nil {
		return m.nowFunc()
	}
	return time.Now()
}

// load reads the persistence file and loads valid sessions into memory.
func (m *Manager) load() error {
	data, err := os.ReadFile(m.filePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read session file: %w", err)
	}

	var store PersistedStore
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&store); err != nil {
		return fmt.Errorf("invalid session store format: %w", err)
	}
	if dec.Decode(new(any)) != io.EOF {
		return fmt.Errorf("trailing garbage in session store")
	}

	// Validate Version
	if store.Version != 1 {
		return fmt.Errorf("unsupported session store version: %d", store.Version)
	}

	// Verify fingerprint
	if store.Fingerprint != m.fingerprint {
		// Fingerprint mismatch means credentials changed or store is obsolete
		// Discard old sessions and clean up file atomically
		m.sessions = make(map[[32]byte]Session)
		return m.saveLocked()
	}

	now := m.now()
	count := 0
	for _, rec := range store.Sessions {
		exp, err := time.Parse(time.RFC3339Nano, rec.ExpiresAt)
		if err != nil {
			exp, err = time.Parse(time.RFC3339, rec.ExpiresAt)
		}
		if err != nil {
			return fmt.Errorf("invalid session expiry format: %w", err)
		}
		// 有效期不超30天（从当前时间算不能晚于 now + RememberTTL，且不能已过期）
		if !now.Before(exp) {
			continue // Expired
		}
		if exp.Sub(now) > RememberTTL {
			return fmt.Errorf("session expiry exceeds 30 days limit")
		}

		hBytes, err := hex.DecodeString(rec.TokenHashHex)
		if err != nil || len(hBytes) != 32 {
			return fmt.Errorf("invalid session token hash: %w", err)
		}
		var hash [32]byte
		copy(hash[:], hBytes)

		if count < MaxSessions {
			m.sessions[hash] = Session{
				Expires:    exp,
				CSRF:       rec.CSRF,
				RememberMe: true,
				TokenHash:  hash,
			}
			count++
		}
	}
	return nil
}

// saveLocked writes remember_me sessions to disk atomically with 0600 permissions.
// Even when 0 sessions exist, it persists an empty sessions list so any stale data is overwritten cleanly.
func (m *Manager) saveLocked() error {
	if m.filePath == "" {
		return nil
	}

	now := m.now()
	recs := make([]PersistedRecord, 0)
	for _, s := range m.sessions {
		if !s.RememberMe {
			continue
		}
		if !now.Before(s.Expires) {
			continue
		}
		recs = append(recs, PersistedRecord{
			TokenHashHex: hex.EncodeToString(s.TokenHash[:]),
			CSRF:         s.CSRF,
			ExpiresAt:    s.Expires.Format(time.RFC3339Nano),
		})
	}

	store := PersistedStore{
		Version:     1,
		Fingerprint: m.fingerprint,
		Sessions:    recs,
	}
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(m.filePath)
	f, err := os.CreateTemp(dir, ".session-*")
	if err != nil {
		return err
	}
	tmpName := f.Name()
	defer os.Remove(tmpName)

	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, m.filePath)
}

// SweepLocked cleans expired sessions. Caller should hold lock.
func (m *Manager) SweepLocked() error {
	now := m.now()
	var removedRemember bool
	// Snapshot in case saveLocked fails
	snapshot := make(map[[32]byte]Session, len(m.sessions))
	for k, v := range m.sessions {
		snapshot[k] = v
	}

	for k, v := range m.sessions {
		if !now.Before(v.Expires) {
			if v.RememberMe {
				removedRemember = true
			}
			delete(m.sessions, k)
		}
	}
	if removedRemember {
		if err := m.saveLocked(); err != nil {
			m.sessions = snapshot
			return err
		}
	}
	return nil
}

// CreateSession creates a new session token, csrf token, and registers the session.
// If rememberMe is true, it persists the remember session.
// If persistence fails, the in-memory session is rolled back and an error is returned.
func (m *Manager) CreateSession(rememberMe bool) (token string, csrf string, expires time.Time, maxAge int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	_ = m.SweepLocked()

	if len(m.sessions) >= MaxSessions {
		return "", "", time.Time{}, 0, errors.New("too many active sessions")
	}

	tok, err := RandomToken()
	if err != nil {
		return "", "", time.Time{}, 0, err
	}
	c, err := RandomToken()
	if err != nil {
		return "", "", time.Time{}, 0, err
	}

	now := m.now()
	var ttl time.Duration
	if rememberMe {
		ttl = RememberTTL
		maxAge = int(RememberTTL.Seconds())
	} else {
		ttl = DefaultTTL
		maxAge = int(DefaultTTL.Seconds())
	}
	exp := now.Add(ttl)
	hash := sha256.Sum256([]byte(tok))

	sess := Session{
		Expires:    exp,
		CSRF:       c,
		RememberMe: rememberMe,
		TokenHash:  hash,
	}

	m.sessions[hash] = sess
	if rememberMe {
		if err := m.saveLocked(); err != nil {
			delete(m.sessions, hash)
			return "", "", time.Time{}, 0, fmt.Errorf("persist session failed: %w", err)
		}
	}

	return tok, c, exp, maxAge, nil
}

// Get validates a raw cookie token value and returns the session if valid.
func (m *Manager) Get(rawToken string) (Session, bool) {
	if rawToken == "" {
		return Session{}, false
	}
	hash := sha256.Sum256([]byte(rawToken))

	m.mu.Lock()
	defer m.mu.Unlock()

	sess, ok := m.sessions[hash]
	if !ok || !m.now().Before(sess.Expires) {
		if ok {
			delete(m.sessions, hash)
			if sess.RememberMe {
				_ = m.saveLocked()
			}
		}
		return Session{}, false
	}
	return sess, true
}

// Delete removes a session by raw token value. It returns error if persistence fails,
// rolling back in-memory deletion to prevent memory/disk desync.
func (m *Manager) Delete(rawToken string) error {
	if rawToken == "" {
		return nil
	}
	hash := sha256.Sum256([]byte(rawToken))

	m.mu.Lock()
	defer m.mu.Unlock()

	sess, ok := m.sessions[hash]
	if !ok {
		return nil
	}

	delete(m.sessions, hash)
	if sess.RememberMe {
		if err := m.saveLocked(); err != nil {
			m.sessions[hash] = sess // restore snapshot
			return fmt.Errorf("revoke persisted session failed: %w", err)
		}
	}
	return nil
}

// Reset clears all in-memory and persisted sessions (e.g. on password change).
// If persisting fails, it restores the in-memory snapshot and returns error.
func (m *Manager) Reset() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	snapshot := make(map[[32]byte]Session, len(m.sessions))
	for k, v := range m.sessions {
		snapshot[k] = v
	}

	m.sessions = make(map[[32]byte]Session)
	if err := m.saveLocked(); err != nil {
		m.sessions = snapshot
		return fmt.Errorf("reset persisted sessions failed: %w", err)
	}
	return nil
}

// UpdateFingerprint changes credential fingerprint and revokes old persisted sessions.
func (m *Manager) UpdateFingerprint(newFingerprint string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	oldFp := m.fingerprint
	snapshot := make(map[[32]byte]Session, len(m.sessions))
	for k, v := range m.sessions {
		snapshot[k] = v
	}

	m.fingerprint = newFingerprint
	m.sessions = make(map[[32]byte]Session)
	if err := m.saveLocked(); err != nil {
		m.fingerprint = oldFp
		m.sessions = snapshot
		return fmt.Errorf("update fingerprint persisted sessions failed: %w", err)
	}
	return nil
}

// ActiveCount returns total active sessions.
func (m *Manager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

func RandomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// CredentialFingerprint computes a stable fingerprint for a username + password hash.
func CredentialFingerprint(username, hash string) string {
	mac := hmac.New(sha256.New, []byte("portmap-web-fingerprint-v1"))
	mac.Write([]byte(username))
	mac.Write([]byte{0})
	mac.Write([]byte(hash))
	return hex.EncodeToString(mac.Sum(nil))
}
