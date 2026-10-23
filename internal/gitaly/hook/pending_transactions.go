package hook

import (
	"bytes"
	"sync"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/graft/refchange"
)

const (
	// pendingTransactionsTTL is how long the prepared transactions of a git
	// process are kept without any further hook call of that process. A git
	// process that dies between preparing and committing its transaction never
	// reports it again, and its entry expires after this time.
	pendingTransactionsTTL = 15 * time.Minute
	// maxPendingInvocations bounds the number of git processes whose prepared
	// transactions are kept at the same time.
	maxPendingInvocations = 4096
)

// pendingTransactions pairs the two reference transactions that git runs to
// delete a reference that exists in the packed-refs file.
//
// Git deletes such a reference in a transaction of the packed-refs backend that
// it nests into the transaction of the loose backend. The nested transaction
// only holds force deletions (`<zero> <zero> <ref>`), and every reference it
// deletes is also part of the outer transaction, which holds the full record of
// the updates, including the expected old values. The hooks of the two
// transactions run in this order:
//
//	prepared  (packed-refs)
//	prepared  (loose)
//	committed (packed-refs)
//	committed (loose)
//
// The changeset log must record each deletion once, so the committed packed-refs
// transaction is skipped: its updates are recorded with the loose transaction
// that commits right after it. To recognize it, the prepared transactions of
// each git process are queued here, keyed by the invocation value of the hooks
// payload (gitcmd.ChangelogMeta.Invocation), which is unique to one git command.
// The state of a process is dropped as soon as all its prepared transactions are
// committed or aborted, so nothing is remembered across git processes.
type pendingTransactions struct {
	mu          sync.Mutex
	invocations map[string]*pendingInvocation
	now         func() time.Time
}

// pendingInvocation holds the prepared, not yet committed or aborted
// transactions of one git process, in the order git prepared them.
type pendingInvocation struct {
	prepared [][]byte
	touched  time.Time
}

func newPendingTransactions() *pendingTransactions {
	return &pendingTransactions{
		invocations: map[string]*pendingInvocation{},
		now:         time.Now,
	}
}

// prepared queues the changes of a transaction that the git process identified
// by invocation has prepared.
func (p *pendingTransactions) prepared(invocation string, changes []byte) {
	if p == nil || invocation == "" {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.now()
	p.expire(now)

	inv, ok := p.invocations[invocation]
	if !ok {
		if len(p.invocations) >= maxPendingInvocations {
			p.evictOldest()
		}

		inv = &pendingInvocation{}
		p.invocations[invocation] = inv
	}

	inv.prepared = append(inv.prepared, bytes.Clone(changes))
	inv.touched = now
}

// aborted forgets the prepared transaction with the given changes of the git
// process identified by invocation.
func (p *pendingTransactions) aborted(invocation string, changes []byte) {
	if p == nil || invocation == "" {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.remove(invocation, changes)
}

// committed forgets the prepared transaction with the given changes of the git
// process identified by invocation. It reports whether that transaction is the
// packed-refs part of a deletion whose loose part is prepared and commits next.
// The updates of such a transaction must not be recorded, because the loose
// transaction records them with their expected old values.
//
// When in doubt, committed reports false, so an update is never lost: a
// transaction that was not seen in the prepared state, or whose references are
// not all part of the transaction prepared after it, is recorded.
func (p *pendingTransactions) committed(objectHash git.ObjectHash, invocation string, changes []byte) bool {
	if p == nil || invocation == "" {
		return false
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	inv, ok := p.invocations[invocation]
	if !ok {
		return false
	}

	index := inv.index(changes)
	if index < 0 {
		return false
	}

	packedPart := index+1 < len(inv.prepared) &&
		isForceDeletionsOnly(objectHash, bytes.NewReader(changes)) &&
		referencesContained(changes, inv.prepared[index+1])

	p.remove(invocation, changes)

	return packedPart
}

// index returns the position of the first prepared transaction with the given
// changes, or -1 when there is none.
func (inv *pendingInvocation) index(changes []byte) int {
	for i, prepared := range inv.prepared {
		if bytes.Equal(prepared, changes) {
			return i
		}
	}

	return -1
}

// remove drops the first prepared transaction with the given changes of the
// invocation, and the invocation itself when it has no prepared transaction
// left. The caller must hold p.mu.
func (p *pendingTransactions) remove(invocation string, changes []byte) {
	inv, ok := p.invocations[invocation]
	if !ok {
		return
	}

	index := inv.index(changes)
	if index < 0 {
		return
	}

	inv.prepared = append(inv.prepared[:index], inv.prepared[index+1:]...)
	if len(inv.prepared) == 0 {
		delete(p.invocations, invocation)
		return
	}

	inv.touched = p.now()
}

// expire drops the invocations without any hook call for longer than
// pendingTransactionsTTL. The caller must hold p.mu.
func (p *pendingTransactions) expire(now time.Time) {
	for invocation, inv := range p.invocations {
		if now.Sub(inv.touched) > pendingTransactionsTTL {
			delete(p.invocations, invocation)
		}
	}
}

// evictOldest drops the invocation that was touched least recently. The caller
// must hold p.mu.
func (p *pendingTransactions) evictOldest() {
	var oldest string
	var oldestTouched time.Time
	for invocation, inv := range p.invocations {
		if oldest == "" || inv.touched.Before(oldestTouched) {
			oldest, oldestTouched = invocation, inv.touched
		}
	}

	delete(p.invocations, oldest)
}

// referencesContained reports whether every reference that inner updates is
// also updated by outer. Both hold the reference-transaction hook's input, one
// `<old> <new> <ref>` line per update.
func referencesContained(inner, outer []byte) bool {
	outerRefs := map[string]struct{}{}
	for _, line := range bytes.Split(outer, []byte("\n")) {
		if ref, ok := updatedReference(line); ok {
			outerRefs[ref] = struct{}{}
		}
	}

	for _, line := range bytes.Split(inner, []byte("\n")) {
		ref, ok := updatedReference(line)
		if !ok {
			continue
		}

		if _, ok := outerRefs[ref]; !ok {
			return false
		}
	}

	return true
}

// updatedReference returns the reference of one line of the
// reference-transaction hook's input.
func updatedReference(line []byte) (string, bool) {
	_, _, reference, ok := refchange.ParseHookLine(string(line))
	return reference, ok
}
