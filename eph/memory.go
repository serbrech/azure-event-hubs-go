package eph

import (
	"context"
	"time"

	"github.com/pkg/errors"
)

type (
	memoryLeaser struct {
		leases        map[string]*memoryLease
		ownerName     string
		leaseDuration time.Duration
	}

	memoryCheckpointer struct {
		checkpoints map[string]*Checkpoint
		processor   *EventProcessorHost
	}

	memoryLease struct {
		Lease
		expirationTime time.Time
	}
)

func newMemoryLease(partitionID string) *memoryLease {
	lease := new(memoryLease)
	lease.PartitionID = partitionID
	return lease
}

// IsNotOwnedOrExpired indicates that the lease has expired and does not owned by a processor
func (l *memoryLease) isNotOwnedOrExpired(ctx context.Context) bool {
	return l.IsExpired(ctx) || l.Owner == ""
}

// IsExpired indicates that the lease has expired and is no longer valid
func (l *memoryLease) IsExpired(_ context.Context) bool {
	return time.Now().After(l.expirationTime)
}

func (l *memoryLease) expireAfter(d time.Duration) {
	l.expirationTime = time.Now().Add(d)
}

func newMemoryLeaser(leaseDuration time.Duration) Leaser {
	return &memoryLeaser{
		leaseDuration: leaseDuration,
	}
}

func (ml *memoryLeaser) SetEventHostProcessor(eph *EventProcessorHost) {
	ml.ownerName = eph.name
}

func (ml *memoryLeaser) StoreExists(ctx context.Context) (bool, error) {
	return ml.leases != nil, nil
}

func (ml *memoryLeaser) EnsureStore(ctx context.Context) error {
	if ml.leases == nil {
		ml.leases = make(map[string]*memoryLease)
	}
	return nil
}

func (ml *memoryLeaser) DeleteStore(ctx context.Context) error {
	return ml.EnsureStore(ctx)
}

func (ml *memoryLeaser) GetLeases(ctx context.Context) ([]LeaseMarker, error) {
	leases := make([]LeaseMarker, len(ml.leases))
	count := 0
	for _, lease := range ml.leases {
		leases[count] = lease
		count++
	}
	return leases, nil
}

func (ml *memoryLeaser) EnsureLease(ctx context.Context, partitionID string) (LeaseMarker, error) {
	l, ok := ml.leases[partitionID]
	if !ok {
		l = newMemoryLease(partitionID)
		ml.leases[l.PartitionID] = l
	}
	return l, nil
}

func (ml *memoryLeaser) DeleteLease(ctx context.Context, partitionID string) error {
	delete(ml.leases, partitionID)
	return nil
}

func (ml *memoryLeaser) AcquireLease(ctx context.Context, partitionID string) (LeaseMarker, bool, error) {
	l, ok := ml.leases[partitionID]
	if !ok {
		// lease is not in store
		return nil, false, errors.New("lease is not in the store")
	}

	if l.isNotOwnedOrExpired(ctx) || l.Owner != ml.ownerName {
		// no one owns it or owned by someone else
		l.Owner = ml.ownerName
	}
	l.expireAfter(ml.leaseDuration)
	return l, true, nil
}

func (ml *memoryLeaser) RenewLease(ctx context.Context, partitionID string) (LeaseMarker, bool, error) {
	l, ok := ml.leases[partitionID]
	if !ok {
		// lease is not in store
		return nil, false, errors.New("lease is not in the store")
	}

	if l.Owner != ml.ownerName {
		return nil, false, nil
	}

	l.expireAfter(ml.leaseDuration)
	return l, true, nil
}

func (ml *memoryLeaser) ReleaseLease(ctx context.Context, partitionID string) (bool, error) {
	l, ok := ml.leases[partitionID]
	if !ok {
		// lease is not in store
		return false, errors.New("lease is not in the store")
	}

	if l.IsExpired(ctx) || l.Owner != ml.ownerName {
		return false, nil
	}

	l.Owner = ""
	l.expirationTime = time.Now().Add(-1 * time.Second)

	return false, nil
}

func (ml *memoryLeaser) UpdateLease(ctx context.Context, partitionID string) (LeaseMarker, bool, error) {
	l, ok, err := ml.RenewLease(ctx, partitionID)
	if err != nil || !ok {
		return nil, ok, err
	}
	l.IncrementEpoch()
	return l, true, nil
}

func (mc *memoryCheckpointer) SetEventHostProcessor(eph *EventProcessorHost) {
	// no op
}

func (mc *memoryCheckpointer) StoreExists(ctx context.Context) (bool, error) {
	return mc.checkpoints == nil, nil
}

func (mc *memoryCheckpointer) EnsureStore(ctx context.Context) error {
	if mc.checkpoints == nil {
		mc.checkpoints = make(map[string]*Checkpoint)
	}
	return nil
}

func (mc *memoryCheckpointer) DeleteStore(ctx context.Context) error {
	mc.checkpoints = nil
	return nil
}

func (mc *memoryCheckpointer) GetCheckpoint(ctx context.Context, partitionID string) (Checkpoint, bool) {
	checkpoint, ok := mc.checkpoints[partitionID]
	if !ok {
		return *new(Checkpoint), ok
	}

	return *checkpoint, true
}

func (mc *memoryCheckpointer) EnsureCheckpoint(ctx context.Context, partitionID string) (Checkpoint, error) {
	checkpoint, ok := mc.checkpoints[partitionID]
	if !ok {
		checkpoint = NewCheckpoint(partitionID)
		mc.checkpoints[partitionID] = checkpoint
	}
	return *checkpoint, nil
}

func (mc *memoryCheckpointer) UpdateCheckpoint(ctx context.Context, checkpoint Checkpoint) error {
	if cp, ok := mc.checkpoints[checkpoint.PartitionID]; ok {
		checkpoint.SequenceNumber = cp.SequenceNumber
		checkpoint.Offset = cp.Offset
	}
	return nil
}

func (mc *memoryCheckpointer) DeleteCheckpoint(ctx context.Context, partitionID string) error {
	delete(mc.checkpoints, partitionID)
	return nil
}
