package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// hostAgentTransitionStore is the durability seam for assignment state. A
// transition publishes the Run Ledger identity, Host Lock, and assignment as
// one recoverable journaled operation.
type hostAgentTransitionStore struct {
	dataDir string
}

type prepareHostAgentTransition func(hostAgentAssignmentRecord, string) (hostAgentAssignmentRecord, error)

func newHostAgentTransitionStore(dataDir string) hostAgentTransitionStore {
	return hostAgentTransitionStore{dataDir: dataDir}
}

func (store hostAgentTransitionStore) Commit(record hostAgentAssignmentRecord, prepare prepareHostAgentTransition) error {
	persistErr := store.persist(record)
	if persistErr == nil {
		return nil
	}
	recoverErr := store.Recover(prepare)
	verifyErr := store.verify(record)
	if recoverErr == nil && verifyErr == nil {
		return nil
	}
	return errors.Join(persistErr, recoverErr, verifyErr)
}

func (store hostAgentTransitionStore) Recover(prepare prepareHostAgentTransition) error {
	root := filepath.Join(store.dataDir, "assignment-transactions")
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return fmt.Errorf("invalid Host Agent transaction path %s", entry.Name())
		}
		path := filepath.Join(root, entry.Name())
		var record hostAgentAssignmentRecord
		if err := readPrivateJSON(path, &record); err != nil {
			return err
		}
		record, err = prepare(record, entry.Name())
		if err != nil {
			return err
		}
		if err := store.apply(record); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		if err := syncRunLedgerDirectory(root); err != nil {
			return err
		}
	}
	return nil
}

func (store hostAgentTransitionStore) SaveAssignment(record hostAgentAssignmentRecord) error {
	return writePrivateJSON(store.assignmentPath(record.RunID), record)
}

func (store hostAgentTransitionStore) persist(record hostAgentAssignmentRecord) error {
	transactionPath := store.transactionPath(record.RunID)
	if err := writePrivateJSON(transactionPath, record); err != nil {
		return err
	}
	if err := store.apply(record); err != nil {
		return err
	}
	if err := os.Remove(transactionPath); err != nil {
		return err
	}
	return syncRunLedgerDirectory(filepath.Dir(transactionPath))
}

func (store hostAgentTransitionStore) verify(want hostAgentAssignmentRecord) error {
	var assignment hostAgentAssignmentRecord
	if err := readPrivateJSON(store.assignmentPath(want.RunID), &assignment); err != nil {
		return err
	}
	if assignment.State != want.State || assignment.AgentID != want.AgentID || assignment.HostID != want.HostID || !sameLockToken(assignment.LockToken, want.LockToken) || assignment.hostLockDeadlines != want.hostLockDeadlines || assignment.ExpiryFromState != want.ExpiryFromState || assignment.ExpiryReason != want.ExpiryReason || !slices.Equal(assignment.DeadlineEvents, want.DeadlineEvents) || assignment.SessionBindingRequired != want.SessionBindingRequired || !sameBrokerSession(assignment.BrokerSession, want.BrokerSession) {
		return fmt.Errorf("durable Host Agent assignment does not match transition")
	}
	ledgerStore := newRunLedgerStore(store.runRepoDir(want.RunID))
	if want.State == "completed" {
		if want.TerminalLedger == nil {
			return fmt.Errorf("completed Host Agent transition is missing its terminal Run Ledger")
		}
		live, err := ledgerStore.Read(want.RunID)
		if err != nil || live.State != want.TerminalLedger.State || live.Status != want.TerminalLedger.Status || live.AcceptedAt != want.TerminalLedger.AcceptedAt {
			return errors.Join(fmt.Errorf("completed Host Agent transition did not publish its terminal Run Ledger"), err)
		}
		if _, err := os.Lstat(store.hostLockPath(want.HostID)); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(fmt.Errorf("completed Host Agent transition retained its Host Lock"), err)
		}
		return nil
	}
	var lock hostAgentLockRecord
	if err := readPrivateJSON(store.hostLockPath(want.HostID), &lock); err != nil {
		return err
	}
	if lock.State != want.State || lock.RunID != want.RunID || lock.AgentID != want.AgentID || !sameLockToken(lock.LockToken, want.LockToken) || lock.hostLockDeadlines != want.hostLockDeadlines || lock.ExpiryFromState != want.ExpiryFromState || lock.ExpiryReason != want.ExpiryReason || lock.SessionBindingRequired != want.SessionBindingRequired || !sameBrokerSession(lock.BrokerSession, want.BrokerSession) {
		return fmt.Errorf("durable Host Lock does not match transition")
	}
	if want.AssignedLedger != nil {
		live, err := ledgerStore.Read(want.RunID)
		if err != nil || live.Host != want.HostID || live.AcceptedAt != want.AssignedLedger.AcceptedAt {
			return errors.Join(fmt.Errorf("active Host Agent transition did not publish its assigned Run Ledger"), err)
		}
	}
	return nil
}

func (store hostAgentTransitionStore) apply(record hostAgentAssignmentRecord) error {
	ledgerStore := newRunLedgerStore(store.runRepoDir(record.RunID))
	var eventTime time.Time
	if len(record.DeadlineEvents) > 0 {
		var err error
		eventTime, err = time.Parse(time.RFC3339Nano, record.DeadlineEvents[len(record.DeadlineEvents)-1].Timestamp)
		if err != nil {
			return fmt.Errorf("invalid Host Lock deadline event timestamp: %w", err)
		}
	}
	if record.State == "completed" {
		if record.TerminalLedger == nil {
			return fmt.Errorf("completed Host Agent transition is missing its terminal Run Ledger")
		}
		if err := ledgerStore.Replace(*record.TerminalLedger); err != nil {
			return err
		}
		if err := appendHostLockDeadlineEventsToLedger(store.runRepoDir(record.RunID), record.TerminalLedger, record.DeadlineEvents, eventTime); err != nil {
			return err
		}
		if err := store.SaveAssignment(record); err != nil {
			return err
		}
		if err := os.Remove(store.hostLockPath(record.HostID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return syncRunLedgerDirectory(filepath.Join(store.dataDir, "host-locks"))
	}
	if record.AssignedLedger != nil {
		if err := ledgerStore.Replace(*record.AssignedLedger); err != nil {
			return err
		}
		if err := appendHostLockDeadlineEventsToLedger(store.runRepoDir(record.RunID), record.AssignedLedger, record.DeadlineEvents, eventTime); err != nil {
			return err
		}
	}
	if err := store.persistHostLock(record, record.State); err != nil {
		return err
	}
	return store.SaveAssignment(record)
}

func (store hostAgentTransitionStore) persistHostLock(assignment hostAgentAssignmentRecord, state string) error {
	return writePrivateJSON(store.hostLockPath(assignment.HostID), hostAgentLockRecord{
		Version: hostAgentAPIVersion, RunID: assignment.RunID, AgentID: assignment.AgentID, HostID: assignment.HostID,
		LockToken: assignment.LockToken, State: state, CreatedAt: assignment.CreatedAt,
		hostLockDeadlines:      assignment.hostLockDeadlines,
		ExpiryFromState:        assignment.ExpiryFromState,
		ExpiryReason:           assignment.ExpiryReason,
		SessionBindingRequired: assignment.SessionBindingRequired, BrokerSession: assignment.BrokerSession,
	})
}

func (store hostAgentTransitionStore) runRepoDir(runID string) string {
	return filepath.Join(store.dataDir, "runs", runID, "repo")
}

func (store hostAgentTransitionStore) assignmentPath(runID string) string {
	return filepath.Join(store.dataDir, "assignments", runID+".json")
}

func (store hostAgentTransitionStore) transactionPath(runID string) string {
	return filepath.Join(store.dataDir, "assignment-transactions", runID+".json")
}

func (store hostAgentTransitionStore) hostLockPath(hostID string) string {
	return filepath.Join(store.dataDir, "host-locks", hostID+".json")
}
