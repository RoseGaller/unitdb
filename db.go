package tracedb

import (
	"bytes"
	"context"
	"io"
	"log"
	"math"
	"math/rand"
	"os"
	"sync"
	"time"

	"github.com/allegro/bigcache"
	"github.com/golang/snappy"
	"github.com/saffat-in/tracedb/crypto"
	fltr "github.com/saffat-in/tracedb/filter"
	"github.com/saffat-in/tracedb/fs"
	"github.com/saffat-in/tracedb/hash"
	"github.com/saffat-in/tracedb/message"
)

const (
	entriesPerBlock = 22
	loadFactor      = 0.7
	// MaxBlocks       = math.MaxUint32
	indexPostfix  = ".index"
	lockPostfix   = ".lock"
	keySize       = 16
	filterPostfix = ".filter"
	version       = 1 // file format version

	// keyExpirationMaxDur expired keys are deleted from db after durType*keyExpirationMaxDur.
	// For example if durType is Minute and keyExpirationMaxDur then
	// all expired keys are deleted from db in 5 minutes
	keyExpirationMaxDur = 1

	// MaxKeyLength is the maximum size of a key in bytes.
	MaxKeyLength = 1 << 16

	// MaxValueLength is the maximum size of a value in bytes.
	MaxValueLength = 1 << 30

	// MaxKeys is the maximum numbers of keys in the DB.
	MaxKeys = math.MaxUint32

	// Maximum number of records to return
	maxResults = 1024
)

type dbInfo struct {
	level         uint8
	count         uint32
	nBlocks       uint32
	splitBlockIdx uint32
	freeblockOff  int64
	hashSeed      uint32
}

// DB represents the message storage for topic->keys-values.
// All DB methods are safe for concurrent use by multiple goroutines.
type DB struct {
	// Need 64-bit alignment.
	seq        uint64
	mu         sync.RWMutex
	mac        *crypto.MAC
	writeLockC chan struct{}
	// consistent   *hash.Consistent
	filter       Filter
	index        table
	data         dataTable
	lock         fs.LockFile
	metrics      Metrics
	cancelSyncer context.CancelFunc
	syncWrites   bool
	dbInfo
	timeWindow timeWindowBucket
	//batchdb
	*batchdb
	//trie
	trie *message.Trie
	// Close.
	closeW sync.WaitGroup
	closeC chan struct{}
	closed uint32
	closer io.Closer
}

// Open opens or creates a new DB.
func Open(path string, opts *Options) (*DB, error) {
	opts = opts.copyWithDefaults()
	// fileFlag := os.O_CREATE | os.O_RDWR
	fs := opts.FileSystem
	fileMode := os.FileMode(0666)
	lock, needsRecovery, err := fs.CreateLockFile(path+lockPostfix, fileMode)
	if err != nil {
		if err == os.ErrExist {
			err = errLocked
		}
		return nil, err
	}

	index, err := newTable(fs, path+indexPostfix)
	if err != nil {
		return nil, err
	}
	data, err := newTable(fs, path)
	if err != nil {
		return nil, err
	}
	filter, err := newTable(fs, path+filterPostfix)
	if err != nil {
		return nil, err
	}
	cache, err := bigcache.NewBigCache(config)
	if err != nil {
		log.Fatal(err)
	}
	cacheID := uint64(rand.Uint32())<<32 + uint64(rand.Uint32())
	db := &DB{
		index:      index,
		data:       dataTable{table: data},
		timeWindow: newTimeWindowBucket(time.Minute, keyExpirationMaxDur),
		filter:     Filter{table: filter, cache: cache, cacheID: cacheID, filterBlock: fltr.NewFilterGenerator()},
		lock:       lock,
		writeLockC: make(chan struct{}, 1),
		metrics:    newMetrics(),
		dbInfo: dbInfo{
			nBlocks:      1,
			freeblockOff: -1,
		},
		batchdb: &batchdb{},
		trie:    message.NewTrie(),
		// Close
		closeC: make(chan struct{}),
	}

	// Create a new MAC from the key.
	if db.mac, err = crypto.New(opts.EncryptionKey); err != nil {
		return nil, err
	}

	//initbatchdb
	if err = db.initbatchdb(); err != nil {
		return nil, err
	}

	if index.size == 0 {
		if data.size != 0 {
			if err := index.Close(); err != nil {
				logger.Error().Err(err).Str("context", "db.Open")
			}
			if err := data.Close(); err != nil {
				logger.Error().Err(err).Str("context", "db.Open")
			}
			if err := lock.Unlock(); err != nil {
				logger.Error().Err(err).Str("context", "db.Open")
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
				logger.Error().Err(err).Str("context", "db.Open")
			}
			if err := data.Close(); err != nil {
				logger.Error().Err(err).Str("context", "db.Open")
			}
			if err := lock.Unlock(); err != nil {
				logger.Error().Err(err).Str("context", "db.Open")
			}
			return nil, err
		}
	}

	// db.consistent = hash.InitConsistent(int(MaxBlocks/blockSize), int(db.nBlocks))

	if needsRecovery {
		if err := db.recover(); err != nil {
			return nil, err
		}
	}

	// loadTrie loads topic into trie on opening an existing database file.
	db.loadTrie()

	if opts.BackgroundSyncInterval > 0 {
		db.startSyncer(opts.BackgroundSyncInterval)
	} else if opts.BackgroundSyncInterval == -1 {
		db.syncWrites = true
	}

	if opts.BackgroundKeyExpiry {
		db.startExpirer(time.Minute, keyExpirationMaxDur)
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
						logger.Error().Err(err).Str("context", "startSyncer").Msg("Error synchronizing database")
					}
					lastModifications = modifications
				}
				time.Sleep(interval)
			}
		}
	}()
}

func (db *DB) startExpirer(durType time.Duration, maxDur int) {
	ctx, cancel := context.WithCancel(context.Background())
	db.cancelSyncer = cancel
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				db.expireOldEntries()
				time.Sleep(durType * time.Duration(maxDur))
			}
		}
	}()
}

func (db *DB) forEachBlock(startBlockIdx uint32, fillCache bool, cb func(blockHandle) (bool, error)) error {
	off := blockOffset(startBlockIdx)
	t := db.index.FileManager
	for {
		b := blockHandle{cache: db.index.cache, cacheID: db.index.cacheID, table: t, offset: off}
		if err := b.read(fillCache); err != nil {
			return err
		}
		if stop, err := cb(b); stop || err != nil {
			return err
		}
		if b.next == 0 {
			return nil
		}
		off = b.next
		t = db.data.FileManager
		db.metrics.BlockProbes.Add(1)
	}
}

func (db *DB) createOverflowBlock() (*blockHandle, error) {
	off, err := db.data.allocate(blockSize)
	if err != nil {
		return nil, err
	}
	return &blockHandle{table: db.data, offset: off}, nil
}

func (db *DB) writeHeader() error {
	db.data.fb.defrag()
	freeblockOff, err := db.data.fb.write(db.data.table)
	if err != nil {
		return err
	}
	db.dbInfo.freeblockOff = freeblockOff
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
	// if !bytes.Equal(h.signature[:], signature[:]) {
	// 	return errCorrupted
	// }
	db.dbInfo = h.dbInfo
	if readFreeList {
		if err := db.data.fb.read(db.data.table, db.dbInfo.freeblockOff); err != nil {
			return err
		}
	}
	db.dbInfo.freeblockOff = -1
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

func (db *DB) blockIndex(keyHash uint32) uint32 {
	idx := keyHash & ((1 << db.level) - 1)
	if idx < db.splitBlockIdx {
		return keyHash & ((1 << (db.level + 1)) - 1)
	}
	return idx
}

func (db *DB) hash(data []byte) uint32 {
	return hash.WithSalt(data, db.hashSeed)
}

// Items returns a new ItemIterator.
func (db *DB) Items(q *Query) (*ItemIterator, error) {
	topic := new(message.Topic)
	if q.Contract == 0 {
		q.Contract = message.Contract
	}
	//Parse the Key
	topic.ParseKey(q.Topic)
	// Parse the topic
	topic.Parse(q.Contract, true)
	if topic.TopicType == message.TopicInvalid {
		return nil, errBadRequest
	}

	topic.AddContract(q.Contract)
	q.parts = topic.Parts

	// In case of ttl, include it to the query
	if from, until, limit, ok := topic.Last(); ok {
		q.prefix = message.GenPrefix(q.parts, until.Unix())
		q.cutoff = from.Unix()
		q.Limit = limit
		if q.Limit == 0 {
			q.Limit = maxResults // Maximum number of records to return
		}
	}
	q.keys = db.trie.Lookup(q.parts)
	return &ItemIterator{db: db, query: q}, nil
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

// Sync commits the contents of the database to the backing FileSystem; this is effectively a noop for an in-memory database. It must only be called while the database is opened.
func (db *DB) Sync() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.sync()
}

func (db *DB) expireOldEntries() {
	expiredEntries := db.timeWindow.expireOldEntries()
	for _, expiredEntry := range expiredEntries {
		entry := expiredEntry.(entry)
		/// Test filter block if message hash presence
		if !db.filter.Test(uint64(entry.hash)) {
			continue
		}
		db.metrics.Dels.Add(1)
		db.mu.Lock()
		defer db.mu.Unlock()
		b := blockHandle{}
		entryIdx := -1
		err := db.forEachBlock(db.blockIndex(entry.hash), false, func(curb blockHandle) (bool, error) {
			b = curb
			for i := 0; i < entriesPerBlock; i++ {
				e := b.entries[i]
				if e.kvOffset == 0 {
					return b.next == 0, nil
				} else if entry.hash == e.hash {
					entryIdx = i
					return true, nil
				}
			}
			return false, nil
		})
		if entryIdx == -1 || err != nil {
			continue
		}
		e := b.entries[entryIdx]
		val, err := db.data.readTopic(e)
		if err != nil {
			continue
		}
		topic := new(message.Topic)
		topic.Unmarshal(val)
		if ok := db.trie.Remove(topic.Parts, e.hash); ok {
			b.del(entryIdx)
			if err := b.write(); err != nil {
				continue
			}
			db.data.free(e.kvSize(), e.kvOffset)
			db.count--
		}

	}
	if db.syncWrites {
		db.sync()
	}
}

// loadTrie loads topics to the trie from data file
func (db *DB) loadTrie() error {
	it := &TopicIterator{db: db}
	for it.First(); it.Valid(); it.Next() {
		err := it.Error()
		if err != nil {
			logger.Error().Err(err).Str("context", "db.loadTrie")
			return err
		}
		db.trie.Add(it.Topic().Parts(), it.Topic().Depth(), it.Topic().ID())
	}
	return nil
}

func (db *DB) GenID() []byte {
	return message.GenID()
}

// PutEntry sets the entry for the given message. It updates the value for the existing message id.
func (db *DB) PutEntry(e *Entry) error {
	// start := time.Now()
	// defer log.Printf("db.Put %d", time.Since(start).Nanoseconds())
	topic := new(message.Topic)
	if e.Contract == 0 {
		e.Contract = message.Contract
	}
	//Parse the Key
	topic.ParseKey(e.Topic)
	e.Topic = topic.Topic
	// Parse the topic
	topic.Parse(e.Contract, true)
	if topic.TopicType == message.TopicInvalid {
		return errBadRequest
	}
	// In case of ttl, add ttl to the msg and store to the db
	if ttl, ok := topic.TTL(); ok {
		//1410065408 10 sec
		e.ExpiresAt = uint32(time.Now().Add(time.Duration(ttl)).Unix())
	}
	topic.AddContract(e.Contract)
	//message ID is the database key
	var id message.ID
	if e.ID != nil {
		id = message.ID(e.ID)
		id.SetContract(topic.Parts)
	} else {
		id = message.NewID(topic.Parts)
	}
	m, err := e.Marshal()
	if err != nil {
		return err
	}
	val := snappy.Encode(nil, m)
	switch {
	case len(id) > MaxKeyLength:
		return errKeyTooLarge
	case len(val) > MaxValueLength:
		return errValueTooLarge
	}
	if ok := db.trie.Add(topic.Parts, topic.Depth, db.hash(id)); ok {
		if _, _, err := db.put(topic.Marshal(), id, val, e.ExpiresAt); err != nil {
			return err
		}
	}
	if float64(db.count)/float64(db.nBlocks*entriesPerBlock) > loadFactor {
		db.split()
	}
	if db.syncWrites {
		db.sync()
	}
	return nil
}

func (db *DB) put(topic, key, value []byte, expiresAt uint32) (blockOff, kvOff int64, err error) {
	var b *blockHandle
	var originalB *blockHandle
	db.metrics.Puts.Add(1)
	db.mu.Lock()
	defer db.mu.Unlock()
	entryIdx := 0
	keyHash := db.hash(key)
	err = db.forEachBlock(db.blockIndex(keyHash), false, func(curb blockHandle) (bool, error) {
		b = &curb
		for i := 0; i < entriesPerBlock; i++ {
			e := b.entries[i]
			entryIdx = i
			if e.kvOffset == 0 {
				// Found an empty entry.
				return true, nil
			} else if keyHash == e.hash {
				// Key already exists.
				if eKey, err := db.data.readKey(e); bytes.Equal(key, eKey) || err != nil {
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
		return blockOff, kvOff, err
	}

	// Inserting a new item.
	if b.entries[entryIdx].kvOffset == 0 {
		if db.count == MaxKeys {
			return blockOff, kvOff, errFull
		}
		db.count++
	} else {
		defer db.data.free(b.entries[entryIdx].kvSize(), b.entries[entryIdx].kvOffset)
	}

	b.entries[entryIdx] = entry{
		hash:      keyHash,
		topicSize: uint16(len(topic)),
		valueSize: uint32(len(value)),
		expiresAt: expiresAt,
	}
	if kvOff, err = db.data.writeKeyValue(topic, key, value); err != nil {
		return blockOff, kvOff, err
	}
	b.entries[entryIdx].kvOffset = kvOff
	if err := b.write(); err != nil {
		return blockOff, kvOff, err
	}
	if originalB != nil {
		err = originalB.write()
		return blockOff, kvOff, err
	}
	db.filter.Append(uint64(keyHash))
	if expiresAt > 0 {
		db.timeWindow.add(b.entries[entryIdx])
	}
	return b.offset, kvOff, nil
}

func (db *DB) split() error {
	updatedBlockIdx := db.splitBlockIdx
	updatedBlockOff := blockOffset(updatedBlockIdx)
	updatedBlock := entryWriter{
		block: &blockHandle{cache: db.index.cache, cacheID: db.index.cacheID, table: db.index, offset: updatedBlockOff},
	}

	newBlockOff, err := db.index.extend(blockSize)
	if err != nil {
		return err
	}
	newBlock := entryWriter{
		block: &blockHandle{cache: db.index.cache, cacheID: db.index.cacheID, table: db.index, offset: newBlockOff},
	}

	db.splitBlockIdx++
	if db.splitBlockIdx == 1<<db.level {
		db.level++
		db.splitBlockIdx = 0
	}

	var overflowBlocks []int64
	if err := db.forEachBlock(updatedBlockIdx, false, func(curb blockHandle) (bool, error) {
		for j := 0; j < entriesPerBlock; j++ {
			e := curb.entries[j]
			if e.kvOffset == 0 {
				break
			}
			if db.blockIndex(e.hash) == updatedBlockIdx {
				if err := updatedBlock.insert(e, db); err != nil {
					return true, err
				}
			} else {
				if err := newBlock.insert(e, db); err != nil {
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

	// for _, off := range overflowBlocks {
	// 	db.data.free(blockSize, off)
	// }

	if err := newBlock.write(); err != nil {
		return err
	}
	if err := updatedBlock.write(); err != nil {
		return err
	}

	db.nBlocks++
	return nil
}

// DeleteEntry delets an entry from database. you must provide an ID to delete message.
// It is safe to modify the contents of the argument after Delete returns but
// not before.
func (db *DB) DeleteEntry(e *Entry) error {
	if e.ID == nil {
		return errKeyEmpty
	}
	topic := new(message.Topic)
	if e.Contract == 0 {
		e.Contract = message.Contract
	}
	//Parse the Key
	topic.ParseKey(e.Topic)
	e.Topic = topic.Topic
	// Parse the topic
	topic.Parse(e.Contract, true)
	if topic.TopicType == message.TopicInvalid {
		return errBadRequest
	}

	topic.AddContract(e.Contract)
	// message ID is the database key
	id := message.ID(e.ID)
	id.SetContract(topic.Parts)

	h := db.hash(id)
	if ok := db.trie.Remove(topic.Parts, h); ok {
		err := db.delete(id)
		if err != nil {
			return err
		}
	}
	return nil
}

// delete deletes the given key from the DB.
func (db *DB) delete(key []byte) error {
	keyHash := db.hash(key)
	/// Test filter block for the message id presence
	if !db.filter.Test(uint64(keyHash)) {
		return nil
	}
	db.metrics.Dels.Add(1)
	db.mu.Lock()
	defer db.mu.Unlock()
	b := blockHandle{}
	entryIdx := -1
	err := db.forEachBlock(db.blockIndex(keyHash), false, func(curb blockHandle) (bool, error) {
		b = curb
		for i := 0; i < entriesPerBlock; i++ {
			e := b.entries[i]
			if e.kvOffset == 0 {
				return b.next == 0, nil
			} else if keyHash == e.hash {
				eKey, err := db.data.readKey(e)
				if err != nil {
					return true, err
				}
				if bytes.Equal(key, eKey) {
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
	e := b.entries[entryIdx]
	b.del(entryIdx)
	if err := b.write(); err != nil {
		return err
	}
	db.data.free(e.kvSize(), e.kvOffset)
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
