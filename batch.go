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

package unitdb

import (
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/unit-io/bpool"
	"github.com/unit-io/unitdb/hash"
	"github.com/unit-io/unitdb/message"
	"github.com/unit-io/unitdb/uid"
)

// SetOptions sets batch options
func (b *Batch) SetOptions(opts *BatchOptions) {
	b.opts = opts
}

type (
	batchInfo struct {
		entryCount int
	}

	batchIndex struct {
		delFlag bool
		key     uint32 // key is local id unique in batch and used to remove duplicate values from a topic in the batch before writing records into DB
		// topicSize uint16
		offset int64
	}

	// Batch is a write batch.
	Batch struct {
		batchID uid.LID
		opts    *BatchOptions
		managed bool
		grouped bool
		order   int8
		batchInfo
		buffer *bpool.Buffer
		size   int64

		db            *DB
		topics        map[uint64]*message.Topic // map[topicHash]*message.Topic
		index         []batchIndex
		pendingWrites []batchIndex

		// commitComplete is used to signal if batch commit is complete and batch is fully written to write ahead log
		commitW        sync.WaitGroup
		commitComplete chan struct{}
	}
)

// Put adds entry to batch for given topic->key/value.
// Client must provide Topic to the BatchOptions.
// It is safe to modify the contents of the argument after Put returns but not
// before.
func (b *Batch) Put(topic, payload []byte) error {
	return b.PutEntry(NewEntry(topic).WithPayload(payload).WithContract(b.opts.Contract))
}

// PutEntry appends entries to a bacth for given topic->key/value pair.
// It is safe to modify the contents of the argument after Put returns but not
// before.
func (b *Batch) PutEntry(e *Entry) error {
	switch {
	case len(e.Topic) == 0:
		return errTopicEmpty
	case len(e.Topic) > maxTopicLength:
		return errTopicTooLarge
	case len(e.Payload) == 0:
		return errValueEmpty
	case len(e.Payload) > maxValueLength:
		return errValueTooLarge
	}
	if e.Contract == 0 {
		e.Contract = message.MasterContract
	}
	topic, ttl, err := b.db.parseTopic(e.Contract, e.Topic)
	if err != nil {
		return err
	}
	topic.AddContract(e.Contract)
	e.topic.hash = topic.GetHash(e.Contract)
	e.topic.data = topic.Marshal()
	e.encryption = b.opts.Encryption
	if err := b.db.setEntry(e, ttl); err != nil {
		return err
	}
	var key uint32
	if !b.opts.AllowDuplicates {
		key = hash.WithSalt(e.val, topic.GetHashCode())
	}
	if _, ok := b.topics[e.topic.hash]; !ok {
		t := new(message.Topic)
		t.Unmarshal(e.topic.data)
		b.topics[e.topic.hash] = t
		// topic is added to index and data if it is new topic entry
		// or else topic size is set to 0 and it is not packed.
		e.topic.size = uint16(len(e.topic.data))
	}
	data, err := b.db.packEntry(e)
	if err != nil {
		return err
	}

	var scratch [4]byte
	binary.LittleEndian.PutUint32(scratch[0:4], uint32(len(data)+4))

	if _, err := b.buffer.Write(scratch[:]); err != nil {
		return err
	}

	if _, err := b.buffer.Write(data); err != nil {
		return err
	}
	b.index = append(b.index, batchIndex{delFlag: false, key: key, offset: b.size})
	b.size += int64(len(data) + 4)
	b.entryCount++

	return nil
}

// Delete appends delete entry to batch for given key.
// It is safe to modify the contents of the argument after Delete returns but
// not before.
func (b *Batch) Delete(id, topic []byte) error {
	return b.DeleteEntry(NewEntry(topic).WithID(id))
}

// DeleteEntry appends entry for deletion to a batch for given key.
// It is safe to modify the contents of the argument after Delete returns but
// not before.
func (b *Batch) DeleteEntry(e *Entry) error {
	switch {
	case b.opts.Immutable:
		return errImmutable
	case len(e.ID) == 0:
		return errMsgIDEmpty
	case len(e.Topic) == 0:
		return errTopicEmpty
	case len(e.Topic) > maxTopicLength:
		return errTopicTooLarge
	}
	topic, _, err := b.db.parseTopic(e.Contract, e.Topic)
	if err != nil {
		return err
	}
	if e.Contract == 0 {
		e.Contract = message.MasterContract
	}
	topic.AddContract(e.Contract)
	id := message.ID(e.ID)
	id.SetContract(e.Contract)
	e.id = id
	e.seq = id.Sequence()
	key := topic.GetHashCode()
	data, err := b.db.packEntry(e)
	if err != nil {
		return err
	}

	var scratch [4]byte
	binary.LittleEndian.PutUint32(scratch[0:4], uint32(len(data)+4))

	if _, err := b.buffer.Write(scratch[:]); err != nil {
		return err
	}

	if _, err := b.buffer.Write(data); err != nil {
		return err
	}
	b.index = append(b.index, batchIndex{delFlag: true, key: key, offset: b.size})
	b.size += int64(len(data) + 4)
	b.entryCount++
	return nil
}

func (b *Batch) writeInternal(fn func(i int, topicHash uint64, seq uint64, expiresAt uint32, data []byte) error) error {
	if err := b.db.ok(); err != nil {
		return err
	} // // CPU profiling by default
	// defer profile.Start().Stop()
	// start := time.Now()
	// defer logger.Debug().Str("context", "batch.writeInternal").Dur("duration", time.Since(start)).Msg("")

	buf := b.buffer.Bytes()
	var e entry
	for i, index := range b.pendingWrites {
		dataLen := binary.LittleEndian.Uint32(buf[index.offset : index.offset+4])
		entryData, data := buf[index.offset+4:index.offset+entrySize+4], buf[index.offset+4:index.offset+int64(dataLen)]
		if err := e.UnmarshalBinary(entryData); err != nil {
			return err
		}
		if index.delFlag && e.seq != 0 {
			/// Test filter block for presence
			if !b.db.filter.Test(e.seq) {
				return nil
			}
			b.db.delete(e.topicHash, e.seq)
			continue
		}

		// put packed entry without topic hash into memdb
		if err := fn(i, e.topicHash, e.seq, e.expiresAt, data); err != nil {
			return err
		}
	}
	return nil
}

// Write starts writing entries into DB. It returns an error if batch write fails.
func (b *Batch) Write() error {
	// The write happen synchronously.
	b.db.writeLockC <- struct{}{}
	defer func() {
		<-b.db.writeLockC
	}()
	b.uniq()
	if b.grouped {
		// append batch to batchgroup
		b.db.batchQueue <- b
		return nil
	}
	err := b.writeInternal(func(i int, topicHash uint64, seq uint64, expiresAt uint32, data []byte) error {
		blockID := startBlockIndex(seq)
		memseq := b.db.cacheID ^ seq
		if err := b.db.mem.Set(uint64(blockID), memseq, data); err != nil {
			return err
		}
		t, ok := b.topics[topicHash]
		if !ok {
			return errTopicEmpty
		}
		b.db.trie.add(topic{hash: topicHash}, t.Parts, t.Depth)
		return b.db.timeWindow.add(topicHash, winEntry{seq: seq, expiresAt: expiresAt})
	})
	if err != nil {
		return err
	}
	return nil
}

// Commit commits changes to the DB. In batch operation commit is managed and client is not allowed to call Commit.
// On Commit complete batch operation signal to the cliend if the batch is fully commmited to DB.
func (b *Batch) Commit() error {
	_assert(!b.managed, "managed tx commit not allowed")
	if len(b.pendingWrites) == 0 || b.buffer.Size() == 0 {
		b.Abort()
		return nil
	}
	defer func() {
		close(b.commitComplete)
	}()
	if err := b.db.commit(b.Len(), b.buffer); err != nil {
		logger.Error().Err(err).Str("context", "commit").Msgf("Error committing batch")
	}
	return nil
}

//Abort abort is a batch cleanup operation on batch complete
func (b *Batch) Abort() {
	_assert(!b.managed, "managed tx abort not allowed")
	b.Reset()
	b.db.bufPool.Put(b.buffer)
	b.db = nil
}

// Reset resets the batch.
func (b *Batch) Reset() {
	b.entryCount = 0
	b.size = 0
	b.index = b.index[:0]
	b.pendingWrites = b.pendingWrites[:0]
	// b.buffer.Reset()
}

func (b *Batch) uniq() []batchIndex {
	if b.opts.AllowDuplicates {
		b.pendingWrites = append(make([]batchIndex, 0, len(b.index)), b.index...)
		return b.pendingWrites
	}
	type indices struct {
		idx    int
		newidx int
	}
	uniqueSet := make(map[uint32]indices, len(b.index))
	i := 0
	for idx := len(b.index) - 1; idx >= 0; idx-- {
		if _, ok := uniqueSet[b.index[idx].key]; !ok {
			uniqueSet[b.index[idx].key] = indices{idx, i}
			i++
		}
	}

	b.pendingWrites = make([]batchIndex, len(uniqueSet))
	for _, i := range uniqueSet {
		b.pendingWrites[len(uniqueSet)-i.newidx-1] = b.index[i.idx]
	}
	return b.pendingWrites
}

func (b *Batch) append(new *Batch) {
	if new.Len() == 0 {
		return
	}
	off := b.size
	for _, idx := range new.index {
		idx.offset = idx.offset + int64(off)
		b.index = append(b.index, idx)
	}
	b.size += new.size
	b.buffer.Write(new.buffer.Bytes())
}

// _assert will panic with a given formatted message if the given condition is false.
func _assert(condition bool, msg string, v ...interface{}) {
	if !condition {
		panic(fmt.Sprintf("assertion failed: "+msg, v...))
	}
}

// Len returns number of records in the batch.
func (b *Batch) Len() int {
	return len(b.pendingWrites)
}

// setManaged sets batch managed.
func (b *Batch) setManaged() {
	b.managed = true
}

// unsetManaged sets batch unmanaged.
func (b *Batch) unsetManaged() {
	b.managed = false
}

// setGrouped set grouping of multiple batches.
func (b *Batch) setGrouped(g *BatchGroup) {
	b.grouped = true
}

// unsetGrouped unset grouping.
func (b *Batch) unsetGrouped() {
	b.grouped = false
}

func (b *Batch) setOrder(order int8) {
	b.order = order
}
