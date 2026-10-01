package web

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// pendingUploads holds a bank file whose layout has not been mapped yet, while
// an administrator maps it (RECON-02).
//
// In memory, deliberately. The file holds outgoing lines -- groceries, rent --
// and the spec keeps those out of the database entirely; a pending upload is
// also worthless after a restart, when the person simply uploads again. Each
// entry is bound to the account that uploaded it, found by an unguessable
// token, and expires. At most one per account, and a small total, so the
// memory held is bounded by maxPending * MaxBytes.
type pendingUploads struct {
	mu      sync.Mutex
	entries map[string]*pendingUpload
	now     func() time.Time
}

type pendingUpload struct {
	token    string
	userID   int64
	fileName string
	data     []byte
	sha256   string
	created  time.Time
}

const (
	pendingTTL = 30 * time.Minute
	maxPending = 8
)

func newPendingUploads() *pendingUploads {
	return &pendingUploads{entries: map[string]*pendingUpload{}, now: time.Now}
}

// put stores a file for a user, replacing any earlier one of theirs, and
// returns its token.
func (p *pendingUploads) put(userID int64, fileName string, data []byte, sha string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked()
	for t, e := range p.entries {
		if e.userID == userID {
			delete(p.entries, t)
		}
	}
	// Full: drop the oldest. Losing someone's half-mapped upload costs them a
	// re-upload; refusing a new one would cost the same, to the wrong person.
	for len(p.entries) >= maxPending {
		var oldest *pendingUpload
		for _, e := range p.entries {
			if oldest == nil || e.created.Before(oldest.created) {
				oldest = e
			}
		}
		delete(p.entries, oldest.token)
	}
	p.entries[token] = &pendingUpload{
		token: token, userID: userID, fileName: fileName, data: data, sha256: sha, created: p.now(),
	}
	return token, nil
}

// get returns the upload for a token, only to the account that made it.
func (p *pendingUploads) get(token string, userID int64) (*pendingUpload, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked()
	e, ok := p.entries[token]
	if !ok || e.userID != userID {
		return nil, false
	}
	return e, true
}

// drop forgets an upload once it has been imported.
func (p *pendingUploads) drop(token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.entries, token)
}

func (p *pendingUploads) sweepLocked() {
	cutoff := p.now().Add(-pendingTTL)
	for t, e := range p.entries {
		if e.created.Before(cutoff) {
			delete(p.entries, t)
		}
	}
}
