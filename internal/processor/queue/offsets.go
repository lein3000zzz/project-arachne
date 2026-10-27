package queue

import (
	"sync"

	"github.com/twmb/franz-go/pkg/kgo"
)

type topicPartition struct {
	topic     string
	partition int32
}

type trackedRecord struct {
	record *kgo.Record
	done   bool
}

// offsetTracker decides what may be committed. Kafka commits an offset per
// partition, meaning "everything before this is done", while workers finish
// records out of order - so the commit point only advances past a record once it
// and every earlier record in its partition are done.
type offsetTracker struct {
	mu        sync.Mutex
	inFlight  map[topicPartition][]*trackedRecord
	committed map[topicPartition]*kgo.Record
}

func newOffsetTracker() *offsetTracker {
	return &offsetTracker{
		inFlight:  make(map[topicPartition][]*trackedRecord),
		committed: make(map[topicPartition]*kgo.Record),
	}
}

func (t *offsetTracker) track(r *kgo.Record) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tp := topicPartition{r.Topic, r.Partition}
	t.inFlight[tp] = append(t.inFlight[tp], &trackedRecord{record: r})
}

// done matches by record identity, not offset: after a rebalance hands a
// partition back, a late ack from the old delivery must not complete the new one.
func (t *offsetTracker) done(r *kgo.Record) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tp := topicPartition{r.Topic, r.Partition}
	queue := t.inFlight[tp]

	for _, tracked := range queue {
		if tracked.record == r {
			tracked.done = true
			break
		}
	}

	advanced := 0
	for advanced < len(queue) && queue[advanced].done {
		t.committed[tp] = queue[advanced].record
		advanced++
	}

	if advanced > 0 {
		t.inFlight[tp] = queue[advanced:]
	}
}

// take returns the newest committable record per partition and forgets them.
func (t *offsetTracker) take() []*kgo.Record {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]*kgo.Record, 0, len(t.committed))
	for tp, r := range t.committed {
		out = append(out, r)
		delete(t.committed, tp)
	}

	return out
}

// restore puts back commits that failed, unless something newer is already due.
func (t *offsetTracker) restore(records []*kgo.Record) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, r := range records {
		tp := topicPartition{r.Topic, r.Partition}
		if newer, ok := t.committed[tp]; !ok || newer.Offset < r.Offset {
			t.committed[tp] = r
		}
	}
}

// release forgets partitions this consumer no longer owns and returns what was
// still committable in them. Records delivered from them are redelivered to the
// new owner.
func (t *offsetTracker) release(partitions map[string][]int32) []*kgo.Record {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]*kgo.Record, 0)

	for topic, ps := range partitions {
		for _, p := range ps {
			tp := topicPartition{topic, p}
			if r, ok := t.committed[tp]; ok {
				out = append(out, r)
			}

			delete(t.committed, tp)
			delete(t.inFlight, tp)
		}
	}

	return out
}
