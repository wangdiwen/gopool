package gopool

import (
    "sync"
    "time"
)

// Task represents a function that will be executed by a worker.
type task func()

// goPool represents a pool of workers.
type goPool struct {
    workers    []*worker
    maxWorkers int
    minWorkers int
    workerStack []int
    taskQueue chan task
    lock sync.Locker
    cond *sync.Cond
    timeout time.Duration
}

// NewGoPool creates a new pool of workers.
func NewGoPool(maxWorkers int, opts ...Option) *goPool {
    pool := &goPool{
        maxWorkers: maxWorkers,
        minWorkers: maxWorkers, // Set minWorkers to maxWorkers by default
        workers:    make([]*worker, maxWorkers),
        workerStack: make([]int, maxWorkers),
        taskQueue: make(chan task, 1e6),
        lock: new(sync.Mutex),
        timeout: 0,
    }
    for _, opt := range opts {
        opt(pool)
    }
    if pool.cond == nil {
        pool.cond = sync.NewCond(pool.lock)
    }
    for i := 0; i < pool.minWorkers; i++ {
        worker := newWorker()
        pool.workers[i] = worker
        pool.workerStack[i] = i
        worker.start(pool, i)
    }
    go pool.dispatch()
    return pool
}

// AddTask adds a task to the pool.
func (p *goPool) AddTask(t task) {
    p.taskQueue <- t
}

// Release stops all workers and releases resources.
func (p *goPool) Release() {
    close(p.taskQueue)
    p.cond.L.Lock()
    for len(p.workerStack) != p.maxWorkers {
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

func (p *goPool) dispatch() {
    for t := range p.taskQueue {
        p.cond.L.Lock()
        for len(p.workerStack) == 0 {
            p.cond.Wait()
        }
        p.cond.L.Unlock()
        workerIndex := p.popWorker()
        p.workers[workerIndex].taskQueue <- t
        if len(p.taskQueue) > (p.maxWorkers-p.minWorkers)/2+p.minWorkers && len(p.workerStack) < p.maxWorkers {
            worker := newWorker()
            p.workers = append(p.workers, worker)
            p.workerStack = append(p.workerStack, len(p.workers)-1)
            worker.start(p, len(p.workers)-1)
        } else if len(p.taskQueue) < p.minWorkers && len(p.workerStack) > p.minWorkers {
            p.workers = p.workers[:len(p.workers)-1]
            p.workerStack = p.workerStack[:len(p.workerStack)-1]
        }
    }
}
