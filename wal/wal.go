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

package wal

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/unit-io/bpool"
)

const (
	version = 1 // file format version

	logExt     = ".log"
	tmpExt     = ".tmp"
	corruptExt = ".CORRUPT"
)

type (
	// WALInfo provides WAL stats.
	WALInfo struct {
		logCountWritten int64
		logCountApplied int64
		entriesWritten  int64
		entriesApplied  int64
	}
	// WAL write ahead logs to recover db commit failure dues to db crash or other unexpected errors.
	WAL struct {
		// wg is a WaitGroup that allows us to wait for the syncThread to finish to
		// ensure a clean shutdown.
		wg sync.WaitGroup
		mu sync.RWMutex

		WALInfo
		recoveredTimeIDs []int64

		bufPool  *bpool.BufferPool
		logStore *_FileStore

		opts Options

		// close
		closed uint32
	}

	// Options wal options to create new WAL. WAL logs uses cyclic rotation to avoid fragmentation.
	// It allocates free blocks only when log reaches target size.
	Options struct {
		Path       string
		BufferSize int64
		Reset      bool
	}
)

func newWal(opts Options) (wal *WAL, err error) {
	wal = &WAL{
		bufPool: bpool.NewBufferPool(opts.BufferSize, nil),
		opts:    opts,
	}
	wal.logStore, err = openFile(opts.Path, opts.BufferSize)
	if err != nil {
		return wal, err
	}

	if opts.Reset {
		wal.logStore.reset()
		return wal, nil
	}

	wal.recoverWal()

	return wal, nil
}

// recoverWal recovers a WAL for the log written but not released. It also updates free blocks.
func (wal *WAL) recoverWal() {
	wal.recoveredTimeIDs = wal.logStore.all()
}

// Close closes the wal, frees used resources and checks for active
// logs.
func (wal *WAL) Close() error {
	if !wal.setClosed() {
		return errors.New("wal is closed")
	}

	// Make sure sync thread isn't running.
	wal.wg.Wait()

	// fmt.Println("wal.close: WALInfo ", wal.WALInfo)
	wal.logStore.close()

	return nil
}

func (wal *WAL) put(log _LogInfo, data *bpool.Buffer) error {
	log.version = version
	wal.logCountWritten++
	wal.entriesWritten += int64(log.count)

	return wal.logStore.put(log, data)
}

// SignalLogApplied informs the WAL that it is safe to reuse blocks.
func (wal *WAL) SignalLogApplied(timeID int64) error {
	wal.mu.RLock()
	wal.wg.Add(1)
	defer func() {
		wal.wg.Done()
		wal.mu.RUnlock()
	}()

	wal.logCountApplied++
	wal.logStore.del(timeID)

	return nil
}

// Reset removes all persistested logs from log store.
func (wal *WAL) Reset() {
	wal.logStore.reset()
}

// setClosed flag; return true if not already closed.
func (wal *WAL) setClosed() bool {
	if wal == nil {
		return false
	}
	return atomic.CompareAndSwapUint32(&wal.closed, 0, 1)
}

// isClosed Checks whether WAL was closed.
func (wal *WAL) isClosed() bool {
	return atomic.LoadUint32(&wal.closed) != 0
}

// Ok checks read ok status.
func (wal *WAL) ok() error {
	if wal.isClosed() {
		return errors.New("wal is closed.")
	}
	return nil
}

// New will open a WAL. If the previous run did not shut down cleanly, a set of
// log entries will be returned which got committed successfully to the WAL, but
// were never signaled as fully completed.
//
// If no WAL exists, a new one will be created.
//
func New(opts Options) (*WAL, error) {
	// Create a wal
	return newWal(opts)
}
