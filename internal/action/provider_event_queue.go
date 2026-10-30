package action

import (
	"sync/atomic"
)

// providerEventQueue keeps Status latency out of the CLI stdout pipe drain.
type providerEventQueue struct {
	events  chan attemptRecord
	done    chan struct{}
	bytes   atomic.Int64
	failed  atomic.Bool
	stopped atomic.Bool
	limit   int64
}

func newProviderEventQueue(send func(attemptRecord) error) *providerEventQueue {
	queue := &providerEventQueue{events: make(chan attemptRecord, 1024), done: make(chan struct{}), limit: maxAttemptRecordBytes}
	go func() {
		defer close(queue.done)
		for event := range queue.events {
			queue.bytes.Add(-int64(len(event.Text)))
			if queue.stopped.Load() {
				continue
			}
			if err := send(event); err != nil {
				queue.failed.Store(true)
				queue.stopped.Store(true)
			}
		}
	}()
	return queue
}

func (q *providerEventQueue) add(event attemptRecord) {
	if q.stopped.Load() {
		return
	}
	if event.Agent == "" {
		q.failed.Store(true)
		return
	}
	size := int64(len(event.Text))
	if q.bytes.Add(size) > q.limit {
		q.bytes.Add(-size)
		q.failed.Store(true)
		return
	}
	select {
	case q.events <- event:
	default:
		q.bytes.Add(-size)
		q.failed.Store(true)
	}
}

// finish drains emitted events; a gap forbids a coverage marker.
func (q *providerEventQueue) finish() bool {
	close(q.events)
	<-q.done
	return !q.failed.Load()
}
