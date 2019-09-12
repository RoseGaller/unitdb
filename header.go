package tracedb

import (
	"encoding/binary"
)

var (
	signature  = [7]byte{'t', 'r', 'a', 'c', 'e', 'd', 'b'}
	headerSize uint32
)

type header struct {
	signature [7]byte
	version   uint32
	dbInfo
	_ [256]byte
}

func init() {
	headerSize = align512(uint32(binary.Size(header{})))
}

func (h header) MarshalBinary() ([]byte, error) {
	buf := make([]byte, headerSize)
	copy(buf[:8], h.signature[:])
	binary.LittleEndian.PutUint32(buf[8:12], h.version)
	buf[12] = h.level
	binary.LittleEndian.PutUint32(buf[13:17], h.count)
	binary.LittleEndian.PutUint32(buf[17:21], h.nBlocks)
	binary.LittleEndian.PutUint32(buf[21:25], h.splitBlockIdx)
	binary.LittleEndian.PutUint64(buf[25:33], uint64(h.freelistOff))
	binary.LittleEndian.PutUint32(buf[33:37], h.hashSeed)
	return buf, nil
}

func (h *header) UnmarshalBinary(data []byte) error {
	copy(h.signature[:], data[:8])
	h.version = binary.LittleEndian.Uint32(data[8:12])
	h.level = data[12]
	h.count = binary.LittleEndian.Uint32(data[13:17])
	h.nBlocks = binary.LittleEndian.Uint32(data[17:21])
	h.splitBlockIdx = binary.LittleEndian.Uint32(data[21:25])
	h.freelistOff = int64(binary.LittleEndian.Uint64(data[25:33]))
	h.hashSeed = binary.LittleEndian.Uint32(data[33:37])
	return nil
}
