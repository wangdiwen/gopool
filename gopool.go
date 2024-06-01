package gopool

import (
	"context"
	"sort"
	"sync"
	"time"
)

// GoPool represents a pool of workers.
type GoPool interface {
	// AddTask adds a task to the pool.
	AddTask(t Task)
	// Wait waits for all tasks to be dispatched and completed.
	Wait()
	// Release releases the pool and all its workers.
	Release()
	// GetRunning returns the number of running workers.
	Running() int
	// GetWorkerCount returns the number of workers.
	GetWorkerCount() int
	// GetTaskQueueSize returns the size of the task queue.
	GetTaskQueueSize() int
}

// task represents a function that will be executed by a worker.
// It returns a result and an error.
type Task func() (interface{}, error)

// type Task task

// goPool represents a pool of workers.
type goPool struct {
	workers     []*worker
	workerStack []int
	maxWorkers  int
	// Set by WithMinWorkers(), used to adjust the number of workers. Default equals to maxWorkers.
	minWorkers int
	// tasks are added to this channel first, then dispatched to workers. Default buffer size is 1 million.
	taskQueue chan Task
	// Set by WithTaskQueueSize(), used to set the size of the task queue. Default is 1e6.
	taskQueueSize int
	// Set by WithRetryCount(), used to retry a task when it fails. Default is 0.
	retryCount int
	lock       sync.Locker
	cond       *sync.Cond
	// Set by WithTimeout(), used to set a timeout for a task. Default is 0, which means no timeout.
	timeout time.Duration
	// Set by WithResultCallback(), used to handle the result of a task. Default is nil.
	resultCallback func(interface{})
	// Set by WithErrorCallback(), used to handle the error of a task. Default is nil.
	errorCallback func(error)
	// adjustInterval is the interval to adjust the number of workers. Default is 1 second.
	adjustInterval time.Duration
	ctx            context.Context
	// cancel is used to cancel the context. It is called when Release() is called.
	cancel context.CancelFunc
}

// NewGoPool creates a new pool of workers.
func NewGoPool(maxWorkers int, opts ...Option) GoPool {
	ctx, cancel := context.WithCancel(context.Background())
	pool := &goPool{
		maxWorkers: maxWorkers,
		// Set minWorkers to maxWorkers by default
		minWorkers: maxWorkers,
		// workers and workerStack should be initialized after WithMinWorkers() is called
		workers:        nil,
		workerStack:    nil,
		taskQueue:      nil,
		taskQueueSize:  1e6,
		retryCount:     0,
		lock:           new(sync.Mutex),
		timeout:        0,
		adjustInterval: 1 * time.Second,
		ctx:            ctx,
		cancel:         cancel,
	}
	// Apply options
	for _, opt := range opts {
		opt(pool)
	}

	pool.taskQueue = make(chan task, pool.taskQueueSize)
	pool.workers = make([]*worker, pool.minWorkers)
	pool.workerStack = make([]int, pool.minWorkers)

	if pool.cond == nil {
		pool.cond = sync.NewCond(pool.lock)
	}
	// Create workers with the minimum number. Don't use pushWorker() here.
	for i := 0; i < pool.minWorkers; i++ {
		worker := newWorker()
		pool.workers[i] = worker
		pool.workerStack[i] = i
		worker.start(pool, i)
	}
	go pool.adjustWorkers()
	go pool.dispatch()
	return pool
}

// AddTask adds a task to the pool.
func (p *goPool) AddTask(t task) {
	p.taskQueue <- t
}

// Wait waits for all tasks to be dispatched and completed.
func (p *goPool) Wait() {
	for {
		p.lock.Lock()
		workerStackLen := len(p.workerStack)
		p.lock.Unlock()

		if len(p.taskQueue) == 0 && workerStackLen == len(p.workers) {
			break
		}

		time.Sleep(50 * time.Millisecond)
	}
}

// Release stops all workers and releases resources.
func (p *goPool) Release() {
	close(p.taskQueue)
	p.cancel()
	p.cond.L.Lock()
	for len(p.workerStack) != p.minWorkers {
		p.cond.Wait()
	}
	p.cond.L.Unlock()
	for _, worker := range p.workers {
		close(worker.taskQueue)
	}
	p.workers = nil
	p.workerStack = nil
}

func (p *goPool) popWorker() int {
	p.lock.Lock()
	workerIndex := p.workerStack[len(p.workerStack)-1]
	p.workerStack = p.workerStack[:len(p.workerStack)-1]
	p.lock.Unlock()
	return workerIndex
}

func (p *goPool) pushWorker(workerIndex int) {
	p.lock.Lock()
	p.workerStack = append(p.workerStack, workerIndex)
	p.lock.Unlock()
	p.cond.Signal()
}

// adjustWorkers adjusts the number of workers according to the number of tasks in the queue.
func (p *goPool) adjustWorkers() {
	ticker := time.NewTicker(p.adjustInterval)
	defer ticker.Stop()

	var adjustFlag bool

	for {
		adjustFlag = false
		select {
		case <-ticker.C:
			p.cond.L.Lock()
			if len(p.taskQueue) > len(p.workers)*3/4 && len(p.workers) < p.maxWorkers {
				adjustFlag = true
				// Double the number of workers until it reaches the maximum
				newWorkers := min(len(p.workers)*2, p.maxWorkers) - len(p.workers)
				for i := 0; i < newWorkers; i++ {
					worker := newWorker()
					p.workers = append(p.workers, worker)
					// Don't use len(p.workerStack)-1 here, because it will be less than len(p.workers)-1 when the pool is busy
					p.workerStack = append(p.workerStack, len(p.workers)-1)
					worker.start(p, len(p.workers)-1)
				}
			} else if len(p.taskQueue) == 0 && len(p.workerStack) == len(p.workers) && len(p.workers) > p.minWorkers {
				adjustFlag = true
				// Halve the number of workers until it reaches the minimum
				removeWorkers := (len(p.workers) - p.minWorkers + 1) / 2
				// Sort the workerStack before removing workers.
				// [1,2,3,4,5] -working-> [1,2,3] -expansive-> [1,2,3,6,7] -idle-> [1,2,3,6,7,4,5]
				sort.Ints(p.workerStack)
				p.workers = p.workers[:len(p.workers)-removeWorkers]
				p.workerStack = p.workerStack[:len(p.workerStack)-removeWorkers]
			}
			p.cond.L.Unlock()
			if adjustFlag {
				p.cond.Broadcast()
			}
		case <-p.ctx.Done():
			return
		}
	}
}

// dispatch dispatches tasks to workers.
func (p *goPool) dispatch() {
	for t := range p.taskQueue {
		p.cond.L.Lock()
		for len(p.workerStack) == 0 {
			p.cond.Wait()
		}
		p.cond.L.Unlock()
		workerIndex := p.popWorker()
		p.workers[workerIndex].taskQueue <- t
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Running returns the number of workers that are currently working.
func (p *goPool) Running() int {
	p.lock.Lock()
	defer p.lock.Unlock()
	return len(p.workers) - len(p.workerStack)
}

// GetWorkerCount returns the number of workers in the pool.
func (p *goPool) GetWorkerCount() int {
	p.lock.Lock()
	defer p.lock.Unlock()
	return len(p.workers)
}

// GetTaskQueueSize returns the size of the task queue.
func (p *goPool) GetTaskQueueSize() int {
	p.lock.Lock()
	defer p.lock.Unlock()
	return p.taskQueueSize
}
