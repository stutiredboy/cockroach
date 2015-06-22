// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Spencer Kimball (spencer.kimball@gmail.com)

package storage

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/proto"
	"github.com/cockroachdb/cockroach/storage/engine"
	"github.com/cockroachdb/cockroach/util"
	"github.com/cockroachdb/cockroach/util/hlc"
	"github.com/cockroachdb/cockroach/util/leaktest"
	"github.com/cockroachdb/cockroach/util/log"
	"github.com/google/btree"
)

// Test implementation of a range set backed by btree.BTree.
type testRangeSet struct {
	sync.Mutex
	rangesByKey *btree.BTree
	visited     int
}

// newTestRangeSet creates a new range set that has the count number of ranges.
func newTestRangeSet(count int, t *testing.T) *testRangeSet {
	rs := &testRangeSet{rangesByKey: btree.New(64 /* degree */)}
	for i := 0; i < count; i++ {
		desc := &proto.RangeDescriptor{
			RaftID:   proto.RaftID(i),
			StartKey: proto.Key(fmt.Sprintf("%03d", i)),
			EndKey:   proto.Key(fmt.Sprintf("%03d", i+1)),
		}
		// Initialize the range stat so the scanner can use it.
		rng := &Range{
			stats: &rangeStats{
				raftID: desc.RaftID,
				MVCCStats: engine.MVCCStats{
					KeyBytes:  1,
					ValBytes:  2,
					KeyCount:  1,
					LiveCount: 1,
				},
			},
		}
		if err := rng.setDesc(desc); err != nil {
			t.Fatal(err)
		}
		if exRngItem := rs.rangesByKey.ReplaceOrInsert(rng); exRngItem != nil {
			t.Fatalf("failed to insert range %s", rng)
		}
	}
	return rs
}

func (rs *testRangeSet) Visit(visitor func(*Range) bool) {
	rs.Lock()
	defer rs.Unlock()
	rs.visited = 0
	rs.rangesByKey.Ascend(func(i btree.Item) bool {
		rs.visited++
		rs.Unlock()
		defer rs.Lock()
		return visitor(i.(*Range))
	})
}

func (rs *testRangeSet) EstimatedCount() int {
	rs.Lock()
	defer rs.Unlock()
	count := rs.rangesByKey.Len() - rs.visited
	if count < 1 {
		count = 1
	}
	return count
}

// removeRange removes the i-th range from the range set.
func (rs *testRangeSet) remove(index int, t *testing.T) *Range {
	endKey := proto.Key(fmt.Sprintf("%03d", index+1))
	rs.Lock()
	defer rs.Unlock()
	rng := rs.rangesByKey.Delete((rangeBTreeKey)(endKey))
	if rng == nil {
		t.Fatalf("failed to delete range of end key %s", endKey)
	}
	return rng.(*Range)
}

// Test implementation of a range queue which adds range to an
// internal slice.
type testQueue struct {
	sync.Mutex // Protects ranges, done & processed count
	ranges     []*Range
	done       bool
	processed  int
	disabled   bool
}

// setDisabled suspends processing of items from the queue.
func (tq *testQueue) setDisabled(d bool) {
	tq.Lock()
	defer tq.Unlock()
	tq.disabled = d
}

func (tq *testQueue) Start(clock *hlc.Clock, stopper *util.Stopper) {
	stopper.RunWorker(func() {
		for {
			select {
			case <-time.After(1 * time.Millisecond):
				tq.Lock()
				if !tq.disabled && len(tq.ranges) > 0 {
					tq.ranges = tq.ranges[1:]
					tq.processed++
				}
				tq.Unlock()
			case <-stopper.ShouldStop():
				tq.Lock()
				tq.done = true
				tq.Unlock()
				return
			}
		}
	})
}

func (tq *testQueue) MaybeAdd(rng *Range, now proto.Timestamp) {
	tq.Lock()
	defer tq.Unlock()
	if index := tq.indexOf(rng); index == -1 {
		tq.ranges = append(tq.ranges, rng)
	}
}

func (tq *testQueue) MaybeRemove(rng *Range) {
	tq.Lock()
	defer tq.Unlock()
	if index := tq.indexOf(rng); index != -1 {
		tq.ranges = append(tq.ranges[:index], tq.ranges[index+1:]...)
	}
}

func (tq *testQueue) count() int {
	tq.Lock()
	defer tq.Unlock()
	return len(tq.ranges)
}

func (tq *testQueue) indexOf(rng *Range) int {
	for i, r := range tq.ranges {
		if r == rng {
			return i
		}
	}
	return -1
}

func (tq *testQueue) isDone() bool {
	tq.Lock()
	defer tq.Unlock()
	return tq.done
}

// TestScannerAddToQueues verifies that ranges are added to and
// removed from multiple queues.
func TestScannerAddToQueues(t *testing.T) {
	defer leaktest.AfterTest(t)
	const count = 3
	ranges := newTestRangeSet(count, t)
	q1, q2 := &testQueue{}, &testQueue{}
	// We don't want to actually consume entries from the queues during this test.
	q1.setDisabled(true)
	q2.setDisabled(true)
	s := newRangeScanner(1*time.Millisecond, 0, ranges, nil)
	s.AddQueues(q1, q2)
	mc := hlc.NewManualClock(0)
	clock := hlc.NewClock(mc.UnixNano)
	stopper := util.NewStopper()

	// Start queue and verify that all ranges are added to both queues.
	s.Start(clock, stopper)
	if err := util.IsTrueWithin(func() bool {
		return q1.count() == count && q2.count() == count
	}, 50*time.Millisecond); err != nil {
		t.Error(err)
	}

	// Remove first range and verify it does not exist in either range.
	rng := ranges.remove(0, t)
	if err := util.IsTrueWithin(func() bool {
		// This is intentionally inside the loop, otherwise this test races as
		// our removal of the range may be processed before a stray re-queue.
		// Removing on each attempt makes sure we clean this up as we retry.
		s.RemoveRange(rng)
		c1 := q1.count()
		c2 := q2.count()
		log.Infof("q1: %d, q2: %d, wanted: %d", c1, c2, count-1)
		return c1 == count-1 && c2 == count-1
	}, time.Second); err != nil {
		t.Error(err)
	}

	// Stop scanner and verify both queues are stopped.
	stopper.Stop()
	if !q1.isDone() || !q2.isDone() {
		t.Errorf("expected all queues to stop; got %t, %t", q1.isDone(), q2.isDone())
	}
}

// TestScannerTiming verifies that ranges are scanned, regardless
// of how many, to match scanInterval.
func TestScannerTiming(t *testing.T) {
	defer leaktest.AfterTest(t)
	const count = 3
	const runTime = 100 * time.Millisecond
	const maxError = 7500 * time.Microsecond
	durations := []time.Duration{
		10 * time.Millisecond,
		25 * time.Millisecond,
	}
	for i, duration := range durations {
		ranges := newTestRangeSet(count, t)
		q := &testQueue{}
		s := newRangeScanner(duration, 0, ranges, nil)
		s.AddQueues(q)
		mc := hlc.NewManualClock(0)
		clock := hlc.NewClock(mc.UnixNano)
		stopper := util.NewStopper()
		defer stopper.Stop()
		s.Start(clock, stopper)
		time.Sleep(runTime)

		avg := s.avgScan()
		log.Infof("%d: average scan: %s", i, avg)
		if avg.Nanoseconds()-duration.Nanoseconds() > maxError.Nanoseconds() ||
			duration.Nanoseconds()-avg.Nanoseconds() > maxError.Nanoseconds() {
			t.Errorf("expected %s, got %s: exceeds max error of %s", duration, avg, maxError)
		}
	}
}

// TestScannerPaceInterval tests that paceInterval returns the correct interval.
func TestScannerPaceInterval(t *testing.T) {
	defer leaktest.AfterTest(t)
	const count = 3
	durations := []time.Duration{
		30 * time.Millisecond,
		60 * time.Millisecond,
		500 * time.Millisecond,
	}
	// function logs an error when the actual value is not close
	// to the expected value
	logErrorWhenNotCloseTo := func(expected, actual time.Duration) {
		delta := 1 * time.Millisecond
		if actual < expected-delta || actual > expected+delta {
			t.Errorf("Expected duration %s, got %s", expected, actual)
		}
	}
	for _, duration := range durations {
		startTime := time.Now()
		ranges := newTestRangeSet(count, t)
		s := newRangeScanner(duration, 0, ranges, nil)
		interval := s.paceInterval(startTime, startTime)
		logErrorWhenNotCloseTo(duration/count, interval)
		// The range set is empty
		ranges = newTestRangeSet(0, t)
		s = newRangeScanner(duration, 0, ranges, nil)
		interval = s.paceInterval(startTime, startTime)
		logErrorWhenNotCloseTo(duration, interval)
		ranges = newTestRangeSet(count, t)
		s = newRangeScanner(duration, 0, ranges, nil)
		// Move the present to duration time into the future
		interval = s.paceInterval(startTime, startTime.Add(duration))
		logErrorWhenNotCloseTo(0, interval)
	}
}

// TestScannerEmptyRangeSet verifies that an empty range set doesn't busy loop.
func TestScannerEmptyRangeSet(t *testing.T) {
	defer leaktest.AfterTest(t)
	ranges := newTestRangeSet(0, t)
	q := &testQueue{}
	s := newRangeScanner(1*time.Millisecond, 0, ranges, nil)
	s.AddQueues(q)
	mc := hlc.NewManualClock(0)
	clock := hlc.NewClock(mc.UnixNano)
	stopper := util.NewStopper()
	defer stopper.Stop()
	s.Start(clock, stopper)
	time.Sleep(3 * time.Millisecond)
	if count := s.Count(); count > 3 {
		t.Errorf("expected three loops; got %d", count)
	}
}

// TestScannerStats verifies that stats accumulate from all ranges.
func TestScannerStats(t *testing.T) {
	defer leaktest.AfterTest(t)
	const count = 3
	ranges := newTestRangeSet(count, t)
	q := &testQueue{}
	stopper := util.NewStopper()
	defer stopper.Stop()
	s := newRangeScanner(1*time.Millisecond, 0, ranges, nil)
	s.AddQueues(q)
	mc := hlc.NewManualClock(0)
	clock := hlc.NewClock(mc.UnixNano)
	// At start, scanner stats should be blank for MVCC, but have accurate number of ranges.
	if rc := s.Stats().RangeCount; rc != count {
		t.Errorf("range count expected %d; got %d", count, rc)
	}
	if vb := s.Stats().MVCC.ValBytes; vb != 0 {
		t.Errorf("value bytes expected %d; got %d", 0, vb)
	}
	s.Start(clock, stopper)
	// We expect a full run to accumulate stats from all ranges.
	if err := util.IsTrueWithin(func() bool {
		if rc := s.Stats().RangeCount; rc != count {
			return false
		}
		if vb := s.Stats().MVCC.ValBytes; vb != count*2 {
			return false
		}
		return true
	}, 100*time.Millisecond); err != nil {
		t.Error(err)
	}
}
