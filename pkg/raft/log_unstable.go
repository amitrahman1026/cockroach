// This code has been modified from its original form by Cockroach Labs, Inc.
// All modifications are Copyright 2024 Cockroach Labs, Inc.
//
// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package raft

import pb "github.com/cockroachdb/cockroach/pkg/raft/raftpb"

// unstable contains "unstable" log entries and snapshot state that has
// not yet been written to Storage. The type serves two roles. First, it
// holds on to new log entries and an optional snapshot until they are
// handed to a Ready struct for persistence. Second, it continues to
// hold on to this state after it has been handed off to provide raftLog
// with a view of the in-progress log entries and snapshot until their
// writes have been stabilized and are guaranteed to be reflected in
// queries of Storage. After this point, the corresponding log entries
// and/or snapshot can be cleared from unstable.
//
// unstable.entries[i] has raft log position i+unstable.offset.
// Note that unstable.offset may be less than the highest log
// position in storage; this means that the next write to storage
// might need to truncate the log before persisting unstable.entries.
type unstable struct {
	// the incoming unstable snapshot, if any.
	//
	// Invariants:
	//	- snapshot == nil ==> !snapshotInProgress
	//	- snapshot != nil ==> offset == snapshot.Metadata.Index + 1
	//
	// The last invariant enforces the order of handling a situation when there is
	// both a snapshot and entries. The snapshot must be acknowledged first,
	// before entries are acknowledged and offset moves forward.
	snapshot *pb.Snapshot
	// all entries that have not yet been written to storage.
	entries []pb.Entry
	// entries[i] has raft log position i+offset.
	offset uint64

	// if true, snapshot is being written to storage.
	//
	// Invariant: snapshotInProgress ==> snapshot != nil
	snapshotInProgress bool
	// entries[:offsetInProgress-offset] are being written to storage.
	// Like offset, offsetInProgress is exclusive, meaning that it
	// contains the index following the largest in-progress entry.
	//
	// Invariant: offset <= offsetInProgress
	// Invariant: offsetInProgress - offset <= len(entries)
	// Invariant: offsetInProgress > offset ==> snapshot == nil || snapshotInProgress
	//
	// The last invariant enforces the order of handling a situation when there is
	// both a snapshot and entries. The snapshot must be sent to storage first.
	offsetInProgress uint64

	logger Logger
}

func newUnstable(offset uint64, logger Logger) unstable {
	return unstable{
		offset:           offset,
		offsetInProgress: offset,
		logger:           logger,
	}
}

// maybeFirstIndex returns the index of the first possible entry in entries
// if it has a snapshot.
func (u *unstable) maybeFirstIndex() (uint64, bool) {
	if u.snapshot != nil {
		return u.snapshot.Metadata.Index + 1, true
	}
	return 0, false
}

// maybeLastIndex returns the last index if it has at least one
// unstable entry or snapshot.
func (u *unstable) maybeLastIndex() (uint64, bool) {
	if l := len(u.entries); l != 0 {
		return u.offset + uint64(l) - 1, true
	}
	if u.snapshot != nil {
		return u.snapshot.Metadata.Index, true
	}
	return 0, false
}

// maybeTerm returns the term of the entry at index i, if there
// is any.
func (u *unstable) maybeTerm(i uint64) (uint64, bool) {
	if i < u.offset {
		if u.snapshot != nil && u.snapshot.Metadata.Index == i {
			return u.snapshot.Metadata.Term, true
		}
		return 0, false
	}

	last, ok := u.maybeLastIndex()
	if !ok {
		return 0, false
	}
	if i > last {
		return 0, false
	}

	return u.entries[i-u.offset].Term, true
}

// nextEntries returns the unstable entries that are not already in the process
// of being written to storage.
func (u *unstable) nextEntries() []pb.Entry {
	inProgress := int(u.offsetInProgress - u.offset)
	if len(u.entries) == inProgress {
		return nil
	}
	return u.entries[inProgress:]
}

// nextSnapshot returns the unstable snapshot, if one exists that is not already
// in the process of being written to storage.
func (u *unstable) nextSnapshot() *pb.Snapshot {
	if u.snapshot == nil || u.snapshotInProgress {
		return nil
	}
	return u.snapshot
}

// acceptInProgress marks all entries and the snapshot, if any, in the unstable
// as having begun the process of being written to storage. The entries/snapshot
// will no longer be returned from nextEntries/nextSnapshot. However, new
// entries/snapshots added after a call to acceptInProgress will be returned
// from those methods, until the next call to acceptInProgress.
func (u *unstable) acceptInProgress() {
	if u.snapshot != nil {
		u.snapshotInProgress = true
	}
	if len(u.entries) > 0 {
		// NOTE: +1 because offsetInProgress is exclusive, like offset.
		u.offsetInProgress = u.entries[len(u.entries)-1].Index + 1
	}
}

// stableTo marks entries up to the entry with the specified (index, term) as
// being successfully written to stable storage.
//
// The method should only be called when the caller can attest that the entries
// can not be overwritten by an in-progress log append. See the related comment
// in newStorageAppendRespMsg.
func (u *unstable) stableTo(id entryID) {
	gt, ok := u.maybeTerm(id.index)
	if !ok {
		// Unstable entry missing. Ignore.
		u.logger.Infof("entry at index %d missing from unstable log; ignoring", id.index)
		return
	}
	if id.index < u.offset {
		// Index matched unstable snapshot, not unstable entry. Ignore.
		u.logger.Infof("entry at index %d matched unstable snapshot; ignoring", id.index)
		return
	}
	if gt != id.term {
		// Term mismatch between unstable entry and specified entry. Ignore.
		// This is possible if part or all of the unstable log was replaced
		// between that time that a set of entries started to be written to
		// stable storage and when they finished.
		u.logger.Infof("entry at (index,term)=(%d,%d) mismatched with "+
			"entry at (%d,%d) in unstable log; ignoring", id.index, id.term, id.index, gt)
		return
	}
	if u.snapshot != nil {
		u.logger.Panicf("entry %+v acked earlier than the snapshot(in-progress=%t): %s",
			id, u.snapshotInProgress, DescribeSnapshot(*u.snapshot))
	}
	num := int(id.index + 1 - u.offset)
	u.entries = u.entries[num:]
	u.offset = id.index + 1
	u.offsetInProgress = max(u.offsetInProgress, u.offset)
	u.shrinkEntriesArray()
}

// shrinkEntriesArray discards the underlying array used by the entries slice
// if most of it isn't being used. This avoids holding references to a bunch of
// potentially large entries that aren't needed anymore. Simply clearing the
// entries wouldn't be safe because clients might still be using them.
func (u *unstable) shrinkEntriesArray() {
	// We replace the array if we're using less than half of the space in
	// it. This number is fairly arbitrary, chosen as an attempt to balance
	// memory usage vs number of allocations. It could probably be improved
	// with some focused tuning.
	const lenMultiple = 2
	if len(u.entries) == 0 {
		u.entries = nil
	} else if len(u.entries)*lenMultiple < cap(u.entries) {
		newEntries := make([]pb.Entry, len(u.entries))
		copy(newEntries, u.entries)
		u.entries = newEntries
	}
}

func (u *unstable) stableSnapTo(i uint64) {
	if u.snapshot != nil && u.snapshot.Metadata.Index == i {
		u.snapshot = nil
		u.snapshotInProgress = false
	}
}

func (u *unstable) restore(s snapshot) {
	// TODO(pav-kv): add a safety check making sure the snapshot does not regress
	// the logMark of unstable. The ("accepted term", lastIndex) is the logical
	// clock that must not regress.
	u.offset = s.lastIndex() + 1
	u.offsetInProgress = u.offset
	u.entries = nil
	u.snapshot = &s.snap
	u.snapshotInProgress = false
}

func (u *unstable) truncateAndAppend(ents []pb.Entry) {
	fromIndex := ents[0].Index

	// We do not expect appends at or before the snapshot index.
	//
	// The caller does stronger checks preventing appends at <= commit index, so
	// the check here is redundant. But it's a defense-in-depth guarding the
	// unstable struct invariant: entries begin at snapshot index + 1. The code
	// below does not regress offset beyond the snapshot.
	if u.snapshot != nil && fromIndex <= u.snapshot.Metadata.Index {
		u.logger.Panicf("appending entry %+v before snapshot %s",
			pbEntryID(&ents[0]), DescribeSnapshot(*u.snapshot))
	}

	switch {
	case fromIndex == u.offset+uint64(len(u.entries)):
		// fromIndex is the next index in the u.entries, so append directly.
		u.entries = append(u.entries, ents...)
	case fromIndex <= u.offset:
		u.logger.Infof("replace the unstable entries from index %d", fromIndex)
		// The log is being truncated to before our current offset
		// portion, so set the offset and replace the entries.
		u.entries = ents
		u.offset = fromIndex
		u.offsetInProgress = u.offset
	default:
		// Truncate to fromIndex (exclusive), and append the new entries.
		u.logger.Infof("truncate the unstable entries before index %d", fromIndex)
		keep := u.slice(u.offset, fromIndex) // NB: appending to this slice is safe,
		u.entries = append(keep, ents...)    // and will reallocate/copy it
		// Only in-progress entries before fromIndex are still considered to be
		// in-progress.
		u.offsetInProgress = min(u.offsetInProgress, fromIndex)
	}
}

// slice returns the entries from the unstable log with indexes in the range
// [lo, hi). The entire range must be stored in the unstable log or the method
// will panic. The returned slice can be appended to, but the entries in it must
// not be changed because they are still shared with unstable.
//
// TODO(pavelkalinnikov): this, and similar []pb.Entry slices, may bubble up all
// the way to the application code through Ready struct. Protect other slices
// similarly, and document how the client can use them.
func (u *unstable) slice(lo uint64, hi uint64) []pb.Entry {
	u.mustCheckOutOfBounds(lo, hi)
	// NB: use the full slice expression to limit what the caller can do with the
	// returned slice. For example, an append will reallocate and copy this slice
	// instead of corrupting the neighbouring u.entries.
	return u.entries[lo-u.offset : hi-u.offset : hi-u.offset]
}

// u.offset <= lo <= hi <= u.offset+len(u.entries)
func (u *unstable) mustCheckOutOfBounds(lo, hi uint64) {
	if lo > hi {
		u.logger.Panicf("invalid unstable.slice %d > %d", lo, hi)
	}
	upper := u.offset + uint64(len(u.entries))
	if lo < u.offset || hi > upper {
		u.logger.Panicf("unstable.slice[%d,%d) out of bound [%d,%d]", lo, hi, u.offset, upper)
	}
}
