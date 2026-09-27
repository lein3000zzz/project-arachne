package queue

import (
	"testing"

	"github.com/twmb/franz-go/pkg/kgo"
)

func records(topic string, partition int32, offsets ...int64) []*kgo.Record {
	out := make([]*kgo.Record, len(offsets))
	for i, o := range offsets {
		out[i] = &kgo.Record{Topic: topic, Partition: partition, Offset: o}
	}
	return out
}

func committable(t *testing.T, tr *offsetTracker) map[int32]int64 {
	t.Helper()

	out := map[int32]int64{}
	for _, r := range tr.take() {
		out[r.Partition] = r.Offset
	}
	return out
}

func TestCommitWaitsForEarlierRecords(t *testing.T) {
	tr := newOffsetTracker()
	rs := records("tasks", 0, 10, 11, 12)
	for _, r := range rs {
		tr.track(r)
	}

	tr.done(rs[2])
	tr.done(rs[1])
	if got := committable(t, tr); len(got) != 0 {
		t.Fatalf("offset 10 is still running, nothing may be committed; got %v", got)
	}

	tr.done(rs[0])
	if got := committable(t, tr); got[0] != 12 {
		t.Fatalf("with 10-12 done the commit point is 12, got %v", got)
	}
}

func TestPartitionsAdvanceIndependently(t *testing.T) {
	tr := newOffsetTracker()
	p0, p1 := records("tasks", 0, 5, 6), records("tasks", 1, 7)
	for _, r := range append(p0, p1...) {
		tr.track(r)
	}

	tr.done(p1[0])
	tr.done(p0[1])

	got := committable(t, tr)
	if _, ok := got[0]; ok {
		t.Error("partition 0 still has offset 5 running")
	}
	if got[1] != 7 {
		t.Errorf("partition 1 is fully done at 7, got %v", got)
	}
}

func TestTakeForgetsAndRestoreKeepsTheNewest(t *testing.T) {
	tr := newOffsetTracker()
	rs := records("tasks", 0, 1, 2)
	for _, r := range rs {
		tr.track(r)
	}

	tr.done(rs[0])
	failed := tr.take()
	if got := committable(t, tr); len(got) != 0 {
		t.Fatalf("take must clear what it returned, got %v", got)
	}

	tr.done(rs[1])
	tr.restore(failed)
	if got := committable(t, tr); got[0] != 2 {
		t.Errorf("a restored older commit must not replace a newer one, got %v", got)
	}
}

func TestAckFromAnEarlierDeliveryIsIgnored(t *testing.T) {
	tr := newOffsetTracker()
	old := records("tasks", 0, 3)[0]
	tr.track(old)
	tr.release(map[string][]int32{"tasks": {0}})

	redelivered := records("tasks", 0, 3)[0]
	tr.track(redelivered)
	tr.done(old)

	if got := committable(t, tr); len(got) != 0 {
		t.Errorf("an ack from before the rebalance must not commit the redelivered record, got %v", got)
	}
}

func TestReleaseReturnsCommittableAndDropsTheRest(t *testing.T) {
	tr := newOffsetTracker()
	rs := records("tasks", 0, 1, 2)
	for _, r := range rs {
		tr.track(r)
	}
	tr.done(rs[0])

	out := tr.release(map[string][]int32{"tasks": {0}})
	if len(out) != 1 || out[0].Offset != 1 {
		t.Fatalf("release must hand back the committable offset 1, got %v", out)
	}

	tr.done(rs[1])
	if got := committable(t, tr); len(got) != 0 {
		t.Errorf("a released partition must not be committed any more, got %v", got)
	}
}

func TestDuplicateAckIsHarmless(t *testing.T) {
	tr := newOffsetTracker()
	rs := records("tasks", 0, 1, 2)
	for _, r := range rs {
		tr.track(r)
	}

	tr.done(rs[0])
	tr.done(rs[0])

	if got := committable(t, tr); got[0] != 1 {
		t.Errorf("got %v, want commit point 1", got)
	}
}
