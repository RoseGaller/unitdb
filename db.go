package tracedb

import (
	"bytes"
	"context"
	"io"
	"log"
	"math"
	"os"
	"sync"
	"time"

	"github.com/allegro/bigcache"
	fltr "github.com/frontnet/tracedb/filter"
	"github.com/frontnet/tracedb/fs"
	"github.com/frontnet/tracedb/hash"
)

const (
	entriesPerBlock = 22
	loadFactor      = 0.7
	indexPostfix    = ".index"
	lockPostfix     = ".lock"
	filterPostfix   = ".filter"
	version         = 1 // file format version

	// MaxKeyLength is the maximum size of a key in bytes.
	MaxKeyLength = 1 << 16

	// MaxValueLength is the maximum size of a value in bytes.
	MaxValueLength = 1 << 30

	// MaxKeys is the maximum numbers of keys in the DB.
	MaxKeys = math.MaxUint32
)

type dbInfo struct {
	level         uint8
	count         uint32
	nBlocks       uint32
	splitBlockIdx uint32
	freelistOff   int64
	hashSeed      uint32
}

// DB represents the key-value storage.
// All DB methods are safe for concurrent use by multiple goroutines.
type DB struct {
	// Need 64-bit alignment.
	seq          uint64
	mu           sync.RWMutex
	writeLockC   chan struct{}
	filter       Filter
	index        file
	data         dataFile
	lock         fs.LockFile
	metrics      Metrics
	cancelSyncer context.CancelFunc
	syncWrites   bool
	dbInfo
	//batchdb
	*batchdb
	// Close.
	closeW sync.WaitGroup
	closeC chan struct{}
	closed uint32
	closer io.Closer
}

// Open opens or creates a new DB.
func Open(path string, opts *Options) (*DB, error) {
	opts = opts.copyWithDefaults()
	fileFlag := os.O_CREATE | os.O_RDWR
	fsys := opts.FileSystem
	fileMode := os.FileMode(0666)
	lock, needsRecovery, err := fsys.CreateLockFile(path+lockPostfix, fileMode)
	if err != nil {
		if err == os.ErrExist {
			err = errLocked
		}
		return nil, err
	}

	index, err := openFile(fsys, path+indexPostfix, fileFlag, fileMode)
	if err != nil {
		return nil, err
	}
	data, err := openFile(fsys, path, fileFlag, fileMode)
	if err != nil {
		return nil, err
	}
	filter, err := openFile(fsys, path+filterPostfix, fileFlag, fileMode)
	if err != nil {
		return nil, err
	}
	cache, err := bigcache.NewBigCache(config)
	if err != nil {
		log.Fatal(err)
	}
	db := &DB{
		index:      index,
		data:       dataFile{file: data},
		filter:     Filter{file: filter, cache: cache, filterBlock: fltr.NewFilterGenerator()},
		lock:       lock,
		writeLockC: make(chan struct{}, 1),
		metrics:    newMetrics(),
		dbInfo: dbInfo{
			nBlocks:     1,
			freelistOff: -1,
		},
		batchdb: &batchdb{},
		// Close
		closeC: make(chan struct{}),
	}
	//initbatchdb
	if err = db.initbatchdb(); err != nil {
		return nil, err
	}

	if index.size == 0 {
		if data.size != 0 {
			if err := index.Close(); err != nil {
				logger.Print(err)
			}
			if err := data.Close(); err != nil {
				logger.Print(err)
			}
			if err := lock.Unlock(); err != nil {
				logger.Print(err)
			}
			// Data file exists, but index is missing.
			return nil, errCorrupted
		}
		seed, err := hash.RandSeed()
		if err != nil {
			return nil, err
		}
		db.hashSeed = seed
		if _, err = db.index.extend(headerSize + blockSize); err != nil {
			return nil, err
		}
		if _, err = db.data.extend(headerSize); err != nil {
			return nil, err
		}
		if err := db.writeHeader(); err != nil {
			return nil, err
		}
	} else {
		if err := db.readHeader(!needsRecovery); err != nil {
			if err := index.Close(); err != nil {
				logger.Print(err)
			}
			if err := data.Close(); err != nil {
				logger.Print(err)
			}
			if err := lock.Unlock(); err != nil {
				logger.Print(err)
			}
			return nil, err
		}
	}
	if needsRecovery {
		if err := db.recover(); err != nil {
			return nil, err
		}
	}
	if opts.BackgroundSyncInterval > 0 {
		db.startSyncer(opts.BackgroundSyncInterval)
	} else if opts.BackgroundSyncInterval == -1 {
		db.syncWrites = true
	}
	return db, nil
}

func blockOffset(idx uint32) int64 {
	return int64(headerSize) + (int64(blockSize) * int64(idx))
}

func (db *DB) startSyncer(interval time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	db.cancelSyncer = cancel
	go func() {
		var lastModifications int64
		for {
			select {
			case <-ctx.Done():
				return
			default:
				modifications := db.metrics.Puts.Value() + db.metrics.Dels.Value()
				if modifications != lastModifications {
					if err := db.Sync(); err != nil {
						logger.Printf("Error synchronizing databse: %v", err)
					}
					lastModifications = modifications
				}
				time.Sleep(interval)
			}
		}
	}()
}

func (db *DB) forEachBlock(startBlockIdx uint32, cb func(blockHandle) (bool, error)) error {
	off := blockOffset(startBlockIdx)
	f := db.index.FileManager
	for {
		b := blockHandle{file: f, offset: off}
		if err := b.read(); err != nil {
			return err
		}
		if stop, err := cb(b); stop || err != nil {
			return err
		}
		if b.next == 0 {
			return nil
		}
		off = b.next
		f = db.data.FileManager
		db.metrics.BlockProbes.Add(1)
	}
}

func (db *DB) createOverflowBlock() (*blockHandle, error) {
	off, err := db.data.allocate(blockSize)
	if err != nil {
		return nil, err
	}
	return &blockHandle{file: db.data, offset: off}, nil
}

func (db *DB) writeHeader() error {
	db.data.fl.defrag()
	freelistOff, err := db.data.fl.write(db.data.file)
	if err != nil {
		return err
	}
	db.dbInfo.freelistOff = freelistOff
	h := header{
		signature: signature,
		version:   version,
		dbInfo:    db.dbInfo,
	}
	return db.index.writeMarshalableAt(h, 0)
}

func (db *DB) readHeader(readFreeList bool) error {
	h := &header{}
	if err := db.index.readUnmarshalableAt(h, headerSize, 0); err != nil {
		return err
	}
	if !bytes.Equal(h.signature[:], signature[:]) {
		return errCorrupted
	}
	db.dbInfo = h.dbInfo
	if readFreeList {
		if err := db.data.fl.read(db.data.file, db.dbInfo.freelistOff); err != nil {
			return err
		}
	}
	db.dbInfo.freelistOff = -1
	return nil
}

// Close closes the DB.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	// Wait for all gorotines to exit.
	db.closeW.Wait()

	if db.cancelSyncer != nil {
		db.cancelSyncer()
	}
	if err := db.writeHeader(); err != nil {
		return err
	}
	if err := db.data.Close(); err != nil {
		return err
	}
	if err := db.index.Close(); err != nil {
		return err
	}
	if err := db.lock.Unlock(); err != nil {
		return err
	}
	if err := db.filter.close(); err != nil {
		return err
	}

	var err error
	// Signal all goroutines.
	close(db.closeC)

	if db.closer != nil {
		if err1 := db.closer.Close(); err == nil {
			err = err1
		}
		db.closer = nil
	}

	// Clear memdbs.
	db.clearMems()

	return err
}

func (db *DB) blockIndex(hash uint32) uint32 {
	idx := hash & ((1 << db.level) - 1)
	if idx < db.splitBlockIdx {
		return hash & ((1 << (db.level + 1)) - 1)
	}
	return idx
}

func (db *DB) hash(data []byte) uint32 {
	return hash.WithSalt(data, db.hashSeed)
}

// Get returns the value for the given key stored in the DB or nil if the key doesn't exist.
func (db *DB) Get(key []byte) ([]byte, error) {
	h := db.hash(key)
	db.metrics.Gets.Add(1)
	db.mu.RLock()
	defer db.mu.RUnlock()
	var retValue []byte

	/// Test filter block for presence
	if !db.filter.Test(uint64(h)) {
		return retValue, nil
	}

	err := db.forEachBlock(db.blockIndex(h), func(b blockHandle) (bool, error) {
		for i := 0; i < entriesPerBlock; i++ {
			sl := b.entries[i]
			if sl.kvOffset == 0 {
				return b.next == 0, nil
			} else if h == sl.hash && uint16(len(key)) == sl.keySize {
				slKey, value, err := db.data.readKeyValue(sl)
				if err != nil {
					return true, err
				}
				if bytes.Equal(key, slKey) {
					retValue = value
					return true, nil
				}
				db.metrics.HashCollisions.Add(1)
			}
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return retValue, nil
}

// Has returns true if the DB contains the given key.
func (db *DB) Has(key []byte) (bool, error) {
	h := db.hash(key)
	/// Test filter block for presence
	if !db.filter.Test(uint64(h)) {
		return false, nil
	}
	found := false
	db.mu.RLock()
	defer db.mu.RUnlock()
	err := db.forEachBlock(db.blockIndex(h), func(b blockHandle) (bool, error) {
		for i := 0; i < entriesPerBlock; i++ {
			sl := b.entries[i]
			if sl.kvOffset == 0 {
				return b.next == 0, nil
			} else if h == sl.hash && uint16(len(key)) == sl.keySize {
				slKey, err := db.data.readKey(sl)
				if err != nil {
					return true, err
				}
				if bytes.Equal(key, slKey) {
					found = true
					return true, nil
				}
			}
		}
		return false, nil
	})
	if err != nil {
		return false, err
	}
	return found, nil
}

// Items returns a new ItemIterator.
func (db *DB) Items() *ItemIterator {
	return &ItemIterator{db: db}
}

func (db *DB) sync() error {
	if err := db.data.Sync(); err != nil {
		return err
	}
	if err := db.index.Sync(); err != nil {
		return err
	}
	return nil
}

// Sync commits the contents of the database to the backing FileSystem; this is effectively a noop for an in-bdbory database. It must only be called while the database is opened.
func (db *DB) Sync() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.sync()
}

func (db *DB) put(hash uint32, key []byte, value []byte, expiresAt uint32) error {
	var b *blockHandle
	var originalB *blockHandle
	entryIdx := 0
	err := db.forEachBlock(db.blockIndex(hash), func(curb blockHandle) (bool, error) {
		b = &curb
		for i := 0; i < entriesPerBlock; i++ {
			sl := b.entries[i]
			entryIdx = i
			if sl.kvOffset == 0 {
				// Found an empty entry.
				return true, nil
			} else if hash == sl.hash && uint16(len(key)) == sl.keySize {
				// Key already exists.
				if slKey, err := db.data.readKey(sl); bytes.Equal(key, slKey) || err != nil {
					return true, err
				}
			}
		}
		if b.next == 0 {
			// Couldn't find free space in the current blockHandle, creating a new overflow blockHandle.
			nextBlock, err := db.createOverflowBlock()
			if err != nil {
				return false, err
			}
			b.next = nextBlock.offset
			originalB = b
			b = nextBlock
			entryIdx = 0
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return err
	}

	// Inserting a new item.
	if b.entries[entryIdx].kvOffset == 0 {
		if db.count == MaxKeys {
			return errFull
		}
		db.count++
	} else {
		defer db.data.free(b.entries[entryIdx].kvSize(), b.entries[entryIdx].kvOffset)
	}

	b.entries[entryIdx] = entry{
		hash:      hash,
		keySize:   uint16(len(key)),
		valueSize: uint32(len(value)),
		expiresAt: expiresAt,
	}
	if b.entries[entryIdx].kvOffset, err = db.data.writeKeyValue(key, value); err != nil {
		return err
	}
	if err := b.write(); err != nil {
		return err
	}
	if originalB != nil {
		return originalB.write()
	}
	return nil
}

func (db *DB) split() error {
	updatedBlockIdx := db.splitBlockIdx
	updatedBlockOff := blockOffset(updatedBlockIdx)
	updatedBlock := entryWriter{
		block: &blockHandle{file: db.index, offset: updatedBlockOff},
	}

	newBlockOff, err := db.index.extend(blockSize)
	if err != nil {
		return err
	}
	newBlock := entryWriter{
		block: &blockHandle{file: db.index, offset: newBlockOff},
	}

	db.splitBlockIdx++
	if db.splitBlockIdx == 1<<db.level {
		db.level++
		db.splitBlockIdx = 0
	}

	var overflowBlocks []int64
	if err := db.forEachBlock(updatedBlockIdx, func(curb blockHandle) (bool, error) {
		for j := 0; j < entriesPerBlock; j++ {
			sl := curb.entries[j]
			if sl.kvOffset == 0 {
				break
			}
			if db.blockIndex(sl.hash) == updatedBlockIdx {
				if err := updatedBlock.insert(sl, db); err != nil {
					return true, err
				}
			} else {
				if err := newBlock.insert(sl, db); err != nil {
					return true, err
				}
			}
		}
		if curb.next != 0 {
			overflowBlocks = append(overflowBlocks, curb.next)
		}
		return false, nil
	}); err != nil {
		return err
	}

	for _, off := range overflowBlocks {
		db.data.free(blockSize, off)
	}

	if err := newBlock.write(); err != nil {
		return err
	}
	if err := updatedBlock.write(); err != nil {
		return err
	}

	db.nBlocks++
	return nil
}

// Delete deletes the given key from the DB.
func (db *DB) Delete(key []byte) error {
	h := db.hash(key)
	/// Test filter block for presence
	if !db.filter.Test(uint64(h)) {
		return nil
	}
	db.metrics.Dels.Add(1)
	db.mu.Lock()
	defer db.mu.Unlock()
	b := blockHandle{}
	entryIdx := -1
	err := db.forEachBlock(db.blockIndex(h), func(curb blockHandle) (bool, error) {
		b = curb
		for i := 0; i < entriesPerBlock; i++ {
			sl := b.entries[i]
			if sl.kvOffset == 0 {
				return b.next == 0, nil
			} else if h == sl.hash && uint16(len(key)) == sl.keySize {
				slKey, err := db.data.readKey(sl)
				if err != nil {
					return true, err
				}
				if bytes.Equal(key, slKey) {
					entryIdx = i
					return true, nil
				}
			}
		}
		return false, nil
	})
	if entryIdx == -1 || err != nil {
		return err
	}
	sl := b.entries[entryIdx]
	b.del(entryIdx)
	if err := b.write(); err != nil {
		return err
	}
	db.data.free(sl.kvSize(), sl.kvOffset)
	db.count--
	if db.syncWrites {
		return db.sync()
	}
	return nil
}

// Count returns the number of items in the DB.
func (db *DB) Count() uint32 {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.count
}

// Metrics returns the DB metrics.
func (db *DB) Metrics() Metrics {
	return db.metrics
}

// FileSize returns the total size of the disk storage used by the DB.
func (db *DB) FileSize() (int64, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	var err error
	is, err := db.index.Stat()
	if err != nil {
		return -1, err
	}
	ds, err := db.data.Stat()
	if err != nil {
		return -1, err
	}
	return is.Size() + ds.Size(), nil
}
