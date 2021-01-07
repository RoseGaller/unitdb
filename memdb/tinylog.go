/*
 * Copyright 2020 Saffat Technologies, Ltd.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package memdb

import (
	"fmt"
	"sync"
	"time"
)

// Default settings
const (
	defaultBlockDuration = 1 * time.Second
	defaultWriteInterval = 100 * time.Millisecond
	defaultTimeout       = 2 * time.Second
	defaultPoolCapacity  = 27
	defaultLogCount      = 1
)

type _TinyLog struct {
	mu sync.RWMutex
	id _TimeID
	_TimeID

	managed  bool
	doneChan chan struct{}
}

func (l *_TinyLog) ID() _TimeID {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.id
}

func (l *_TinyLog) timeID() _TimeID {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l._TimeID
}

func (b *_TinyLog) abort() {
	close(b.doneChan)
}

type (
	_LogOptions struct {
		// writeInterval default value is 100ms, setting writeInterval to zero disables writing the log to the WAL.
		writeInterval time.Duration

		// timeout controls how often log pool kill idle jobs.
		//
		// Default value is 2 seconds
		timeout time.Duration

		// blockDuration is used to create new timeID.
		//
		// Default value is defaultBlockDuration.
		blockDuration time.Duration

		// poolCapacity controls size of pre-allocated log queue.
		//
		// Default value is defaultPoolCapacity.
		poolCapacity int

		// logCount controls number of goroutines commiting the log to the WAL.
		//
		// Default value is 1, so logs are sent from single goroutine, this
		// value might need to be bumped under high load.
		logCount int
	}
	_LogPool struct {
		mu         sync.RWMutex
		db         *DB
		opts       *_LogOptions
		tinyLog    *_TinyLog
		writeQueue chan *_TinyLog
		logQueue   chan *_TinyLog
		stop       chan struct{}
		stopOnce   sync.Once
		stopWg     sync.WaitGroup
	}
)

func (src *_LogOptions) withDefaultOptions() *_LogOptions {
	opts := _LogOptions{}
	if src != nil {
		opts = *src
	}
	if opts.poolCapacity < 1 {
		opts.poolCapacity = 1
	}
	if opts.writeInterval == 0 {
		opts.writeInterval = defaultWriteInterval
	}
	if opts.timeout == 0 {
		opts.timeout = defaultTimeout
	}
	if opts.blockDuration == 0 {
		opts.blockDuration = defaultBlockDuration
	}
	if opts.logCount < 1 {
		opts.logCount = defaultLogCount
	}

	return &opts
}

func (p *_LogPool) newTinyLog() {
	timeNow := time.Now().UTC()
	timeID := _TimeID(timeNow.Truncate(p.opts.blockDuration).UnixNano())
	p.db.addTimeBlock(timeID)
	p.db.internal.timeMark.add(timeID)
	p.tinyLog = &_TinyLog{id: _TimeID(timeNow.UnixNano()), _TimeID: timeID, managed: false, doneChan: make(chan struct{})}
}

func (db *DB) newLogPool(opts *_LogOptions) {
	opts = opts.withDefaultOptions()
	pool := &_LogPool{
		db:         db,
		opts:       opts,
		tinyLog:    &_TinyLog{},
		writeQueue: make(chan *_TinyLog, 1),
		logQueue:   make(chan *_TinyLog, opts.poolCapacity),
		stop:       make(chan struct{}),
	}

	pool.newTinyLog()

	// start the write loop
	go pool.writeLoop(opts.writeInterval)

	// start the commit loop
	pool.stopWg.Add(1)
	go pool.commitLoop()

	// start the dispacther
	for i := 0; i < opts.logCount; i++ {
		pool.stopWg.Add(1)
		go pool.dispatch(opts.timeout)
	}

	db.internal.logPool = pool
}

// timeID returns tinyLog timeID.
func (p *_LogPool) timeID() _TimeID {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.tinyLog.timeID()
}

// size returns maximum number of concurrent jobs.
func (p *_LogPool) size() int {
	return p.opts.poolCapacity
}

// stop tells dispatcher to exit, and wether or not complete queued jobs.
func (p *_LogPool) close(wait bool) {
	p.stopOnce.Do(func() {
		// Close write queue and wait for currently running jobs to finish.
		close(p.stop)
	})
	p.stopWg.Wait()
}

// stopWait stops worker pool and wait for all queued jobs to complete.
func (p *_LogPool) closeWait() {
	p.close(true)
}

// write enqueues a log to write.
func (p *_LogPool) write() {
	if p.tinyLog != nil {
		p.writeQueue <- p.tinyLog
	}
}

// writeWait enqueues the log and waits for it to be executed.
func (p *_LogPool) writeWait(tinyLog *_TinyLog) {
	if tinyLog == nil {
		return
	}
	p.writeQueue <- tinyLog
	<-tinyLog.doneChan
}

// writeLoop enqueue the tiny log to the log pool.
func (p *_LogPool) writeLoop(interval time.Duration) {
	var writeC <-chan time.Time

	if interval > 0 {
		writeTicker := time.NewTicker(interval)
		defer writeTicker.Stop()
		writeC = writeTicker.C
	}

	for {
		select {
		case <-p.stop:
			p.write()
			close(p.writeQueue)

			return
		case <-writeC:
			// check buffer pool backoff and capacity for excess memory usage
			// before writing tiny log to the WAL.
			switch {
			case p.db.cap() > 0.7:
				block, ok := p.db.timeBlock(p.db.timeID())
				if !ok {
					break
				}
				block.RLock()
				size := block.data.Size()
				block.RUnlock()
				if size < 1<<20 {
					break
				}
				fallthrough
			default:
				p.mu.Lock()
				p.write()
				p.newTinyLog()
				p.mu.Unlock()
			}
		}
	}
}

// dispatch handles tiny log commit for the jobs in queue.
func (p *_LogPool) dispatch(timeout time.Duration) {
LOOP:
	for {
		select {
		case tinyLog, ok := <-p.writeQueue:
			// Get a buffer from the queue
			if !ok {
				close(p.logQueue)
				p.stopWg.Done()
				return
			}

			// return tinyLog to the pool
			select {
			case p.logQueue <- tinyLog:
			default:
				// pool is full, let GC handle the buffer
				goto WAIT
			}
		}
	}

WAIT:
	// Wait for a while
	time.Sleep(timeout)
	goto LOOP
}

// commitLoop commits the tiny log to the WAL.
func (p *_LogPool) commitLoop() {
	for {
		select {
		case <-p.stop:
			// run queued jobs from the log queue and
			// process it until queue is empty.
			for {
				select {
				case tinyLog, ok := <-p.logQueue:
					if !ok {
						p.stopWg.Done()
						return
					}
					if err := p.db.tinyCommit(tinyLog); err != nil {
						fmt.Println("logPool.tinyCommit: error ", err)
					}
				default:
				}
			}
		case tinyLog := <-p.logQueue:
			if tinyLog != nil {
				if err := p.db.tinyCommit(tinyLog); err != nil {
					fmt.Println("logPool.tinyCommit: error ", err)
				}
			}
		}
	}
}
