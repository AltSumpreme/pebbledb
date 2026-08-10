// Package txn implements a durable two-phase commit decision log for mutation
// batches that span independently atomic range participants.
package txn

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"sort"

	"pebbledb/codec"
	"pebbledb/storage/kv"
)

var (
	ErrInDoubt       = errors.New("distributed txn: commit decision is durable but acknowledgements are incomplete")
	ErrDuplicateID   = errors.New("distributed txn: transaction ID already has different contents")
	ErrMissingMember = errors.New("distributed txn: recovery participant is unavailable")
)

var (
	decisionPrefix = []byte{0x00, 'p', 'd', 'b', '-', '2', 'p', 'c', '-', 'd', 0x01}
	intentPrefix   = []byte{0x00, 'p', 'd', 'b', '-', '2', 'p', 'c', '-', 'i', 0x01}
)

type decision uint8

const (
	preparing decision = iota + 1
	commitDecision
	abortDecision
)

type decisionRecord struct {
	TransactionID string   `json:"transaction_id"`
	Decision      decision `json:"decision"`
	Participants  []string `json:"participants"`
}

type intentRecord struct {
	TransactionID string        `json:"transaction_id"`
	Mutations     []kv.Mutation `json:"mutations"`
}

// Participant durably stages intents and atomically applies them with intent
// removal after a commit decision.
type Participant struct {
	id    string
	store kv.BatchStore
}

func NewParticipant(id string, store kv.BatchStore) (*Participant, error) {
	if id == "" || store == nil {
		return nil, fmt.Errorf("distributed txn: participant ID and store are required")
	}
	return &Participant{id: id, store: store}, nil
}

func (participant *Participant) ID() string { return participant.id }

func (participant *Participant) Prepare(transactionID string, mutations []kv.Mutation) error {
	if transactionID == "" || len(mutations) == 0 {
		return fmt.Errorf("distributed txn: transaction ID and mutations are required")
	}
	record := intentRecord{TransactionID: transactionID, Mutations: cloneMutations(mutations)}
	encoded, err := encode(record)
	if err != nil {
		return err
	}
	key := intentKey(transactionID)
	existing, found, err := participant.store.Get(key)
	if err != nil {
		return err
	}
	if found {
		if bytes.Equal(existing, encoded) {
			return nil
		}
		return ErrDuplicateID
	}
	return participant.store.Apply([]kv.Mutation{{Key: key, Value: encoded}})
}

func (participant *Participant) Commit(transactionID string) error {
	key := intentKey(transactionID)
	value, found, err := participant.store.Get(key)
	if err != nil || !found {
		return err
	}
	var record intentRecord
	if err := decode(value, &record); err != nil {
		return err
	}
	if record.TransactionID != transactionID {
		return fmt.Errorf("distributed txn: intent hash collision")
	}
	mutations := cloneMutations(record.Mutations)
	mutations = append(mutations, kv.Mutation{Key: key, Delete: true})
	return participant.store.Apply(mutations)
}

func (participant *Participant) Abort(transactionID string) error {
	return participant.store.Delete(intentKey(transactionID))
}

type Batch struct {
	Participant *Participant
	Mutations   []kv.Mutation
}

type Coordinator struct{ store kv.BatchStore }

func NewCoordinator(store kv.BatchStore) (*Coordinator, error) {
	if store == nil {
		return nil, fmt.Errorf("distributed txn: coordinator store is required")
	}
	return &Coordinator{store: store}, nil
}

// Commit returns nil only after every participant acknowledges the durable
// commit decision. Transaction IDs must be globally unique. ErrInDoubt means
// recovery must retry; it never means abort.
func (coordinator *Coordinator) Commit(ctx context.Context, transactionID string, batches []Batch) error {
	participants, err := validateBatches(transactionID, batches)
	if err != nil {
		return err
	}
	record := decisionRecord{TransactionID: transactionID, Decision: preparing, Participants: participants}
	if err := coordinator.putDecision(record); err != nil {
		return err
	}
	prepared := make([]*Participant, 0, len(batches))
	for _, batch := range batches {
		if err := ctx.Err(); err != nil {
			return coordinator.abort(transactionID, record, prepared, err)
		}
		if err := batch.Participant.Prepare(transactionID, batch.Mutations); err != nil {
			return coordinator.abort(transactionID, record, prepared, err)
		}
		prepared = append(prepared, batch.Participant)
	}
	record.Decision = commitDecision
	if err := coordinator.putDecision(record); err != nil {
		// The store may have made the commit decision durable even when it
		// reports an I/O error. Never attempt to change that decision to abort;
		// recovery will inspect the record and safely resolve either state.
		return fmt.Errorf("%w: persist commit decision: %v", ErrInDoubt, err)
	}
	for _, participant := range prepared {
		if err := participant.Commit(transactionID); err != nil {
			return fmt.Errorf("%w: participant %s: %v", ErrInDoubt, participant.ID(), err)
		}
	}
	if err := coordinator.store.Delete(decisionKey(transactionID)); err != nil {
		return fmt.Errorf("%w: remove completed decision: %v", ErrInDoubt, err)
	}
	return nil
}

func (coordinator *Coordinator) abort(transactionID string, record decisionRecord, prepared []*Participant, cause error) error {
	record.Decision = abortDecision
	decisionErr := coordinator.putDecision(record)
	var abortErrors []error
	for _, participant := range prepared {
		if err := participant.Abort(transactionID); err != nil {
			abortErrors = append(abortErrors, err)
		}
	}
	if decisionErr == nil && len(abortErrors) == 0 {
		decisionErr = coordinator.store.Delete(decisionKey(transactionID))
	}
	return errors.Join(cause, decisionErr, errors.Join(abortErrors...))
}

// Recover resolves every persisted decision. PREPARING has no commit decision
// and aborts; COMMIT is rolled forward. Records remain until all members ack.
func (coordinator *Coordinator) Recover(ctx context.Context, registry map[string]*Participant) error {
	entries, err := coordinator.store.Scan(decisionPrefix, codec.PrefixEnd(decisionPrefix))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		var record decisionRecord
		if err := decode(entry.Value, &record); err != nil {
			return fmt.Errorf("distributed txn: decode recovery decision: %w", err)
		}
		if err := validateDecisionRecord(record); err != nil {
			return err
		}
		if !bytes.Equal(entry.Key, decisionKey(record.TransactionID)) {
			return fmt.Errorf("distributed txn: decision key does not match transaction ID")
		}
		for _, participantID := range record.Participants {
			participant := registry[participantID]
			if participant == nil {
				return fmt.Errorf("%w: %s", ErrMissingMember, participantID)
			}
			if record.Decision == commitDecision {
				err = participant.Commit(record.TransactionID)
			} else {
				err = participant.Abort(record.TransactionID)
			}
			if err != nil {
				return fmt.Errorf("distributed txn: recover %s: %w", participantID, err)
			}
		}
		if err := coordinator.store.Delete(entry.Key); err != nil {
			return err
		}
	}
	return nil
}

func (coordinator *Coordinator) putDecision(record decisionRecord) error {
	if err := validateDecisionRecord(record); err != nil {
		return err
	}
	encoded, err := encode(record)
	if err != nil {
		return err
	}
	key := decisionKey(record.TransactionID)
	existing, found, err := coordinator.store.Get(key)
	if err != nil {
		return err
	}
	if found {
		var previous decisionRecord
		if err := decode(existing, &previous); err != nil {
			return err
		}
		if previous.TransactionID != record.TransactionID ||
			!sameStrings(previous.Participants, record.Participants) ||
			!validDecisionTransition(previous.Decision, record.Decision) {
			return ErrDuplicateID
		}
	}
	return coordinator.store.Apply([]kv.Mutation{{Key: key, Value: encoded}})
}

func validateDecisionRecord(record decisionRecord) error {
	if record.TransactionID == "" || len(record.Participants) < 2 {
		return fmt.Errorf("distributed txn: invalid decision identity or participants")
	}
	if record.Decision != preparing && record.Decision != commitDecision && record.Decision != abortDecision {
		return fmt.Errorf("distributed txn: invalid decision state %d", record.Decision)
	}
	for index, participant := range record.Participants {
		if participant == "" || (index > 0 && record.Participants[index-1] >= participant) {
			return fmt.Errorf("distributed txn: decision participants are not unique and sorted")
		}
	}
	return nil
}

func validDecisionTransition(previous, next decision) bool {
	if previous == next {
		return true
	}
	return previous == preparing && (next == commitDecision || next == abortDecision)
}

func validateBatches(transactionID string, batches []Batch) ([]string, error) {
	if transactionID == "" || len(batches) < 2 {
		return nil, fmt.Errorf("distributed txn: ID and at least two participant batches are required")
	}
	seen := make(map[string]struct{}, len(batches))
	participants := make([]string, 0, len(batches))
	for _, batch := range batches {
		if batch.Participant == nil || len(batch.Mutations) == 0 {
			return nil, fmt.Errorf("distributed txn: participant and mutations are required")
		}
		if _, duplicate := seen[batch.Participant.ID()]; duplicate {
			return nil, fmt.Errorf("distributed txn: duplicate participant %s", batch.Participant.ID())
		}
		seen[batch.Participant.ID()] = struct{}{}
		participants = append(participants, batch.Participant.ID())
	}
	sort.Strings(participants)
	return participants, nil
}

func decisionKey(transactionID string) []byte { return hashedKey(decisionPrefix, transactionID) }
func intentKey(transactionID string) []byte   { return hashedKey(intentPrefix, transactionID) }

func hashedKey(prefix []byte, transactionID string) []byte {
	hash := sha256.Sum256([]byte(transactionID))
	return append(append([]byte(nil), prefix...), hash[:]...)
}

func encode(value any) ([]byte, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	encoded := append([]byte{1}, payload...)
	var checksum [4]byte
	binary.BigEndian.PutUint32(checksum[:], crc32.ChecksumIEEE(encoded))
	return append(encoded, checksum[:]...), nil
}

func decode(encoded []byte, destination any) error {
	if len(encoded) < 6 || encoded[0] != 1 {
		return fmt.Errorf("invalid record header")
	}
	payload, checksum := encoded[:len(encoded)-4], encoded[len(encoded)-4:]
	if crc32.ChecksumIEEE(payload) != binary.BigEndian.Uint32(checksum) {
		return fmt.Errorf("record checksum mismatch")
	}
	return json.Unmarshal(payload[1:], destination)
}

func cloneMutations(mutations []kv.Mutation) []kv.Mutation {
	result := make([]kv.Mutation, len(mutations))
	for index, mutation := range mutations {
		result[index] = kv.Mutation{Key: append([]byte(nil), mutation.Key...), Value: append([]byte(nil), mutation.Value...), Delete: mutation.Delete}
	}
	return result
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
