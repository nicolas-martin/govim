package queue

import (
	"sync"
)

type Queue struct {
	work    []func()
	lock    sync.Mutex
	gotwork chan struct{}
}

func NewQueue() *Queue {
	res := &Queue{
		gotwork: make(chan struct{}),
	}
	return res
}

func (q *Queue) GotWork() <-chan struct{} {
	return q.gotwork
}

func (q *Queue) Get() (work func(), ok bool) {
	q.lock.Lock()
	defer q.lock.Unlock()
	if ok = len(q.work) > 0; ok {
		work, q.work = q.work[0], q.work[1:]
	}
	return
}

func (q *Queue) Add(f func()) {
	q.lock.Lock()
	q.work = append(q.work, f)
	go q.signalWork()
	q.lock.Unlock()
}

func (q *Queue) Set(f func()) {
	q.lock.Lock()
	q.work = []func(){f}
	go q.signalWork()
	q.lock.Unlock()
}

func (q *Queue) signalWork() {
	q.gotwork <- struct{}{}
}