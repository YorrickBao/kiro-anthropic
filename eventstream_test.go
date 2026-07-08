package main

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Note on scope: esFrame computes CRCs with crc32.ChecksumIEEE, the same
// function the decoder validates against. These tests therefore verify the
// framing/offset logic and that CRC *corruption is detected* — they do NOT
// independently prove IEEE is the polynomial AWS uses. That is confirmed by
// real end-to-end runs against runtime.kiro.dev.

// --- frame construction helpers (mirror the decoder's expected layout) ---

func esStringHeader(name, value string) []byte {
	b := []byte{byte(len(name))}
	b = append(b, name...)
	b = append(b, 7) // value type 7 = string
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(value)))
	b = append(b, l[:]...)
	b = append(b, value...)
	return b
}

func esInt32Header(name string, v int32) []byte {
	b := []byte{byte(len(name))}
	b = append(b, name...)
	b = append(b, 4) // value type 4 = int32
	var val [4]byte
	binary.BigEndian.PutUint32(val[:], uint32(v))
	b = append(b, val[:]...)
	return b
}

func esFrame(headers []byte, payload []byte) []byte {
	total := 12 + len(headers) + len(payload) + 4
	var prelude [12]byte
	binary.BigEndian.PutUint32(prelude[0:4], uint32(total))
	binary.BigEndian.PutUint32(prelude[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(prelude[8:12], crc32.ChecksumIEEE(prelude[0:8]))

	buf := make([]byte, 0, total)
	buf = append(buf, prelude[:]...)
	buf = append(buf, headers...)
	buf = append(buf, payload...)
	// message CRC covers everything before it (prelude + headers + payload).
	var crc [4]byte
	binary.BigEndian.PutUint32(crc[:], crc32.ChecksumIEEE(buf))
	buf = append(buf, crc[:]...)
	return buf
}

func TestEventStreamDecodeSingle(t *testing.T) {
	headers := append(esStringHeader(":message-type", "event"),
		esStringHeader(":event-type", "assistantResponseEvent")...)
	payload := []byte(`{"content":"hello"}`)
	frame := esFrame(headers, payload)

	d := newEventStreamDecoder(bytes.NewReader(frame))
	msg, err := d.Next()
	require.NoError(t, err)
	assert.Equal(t, "assistantResponseEvent", msg.eventType())
	assert.Equal(t, "event", msg.messageType())
	assert.Equal(t, `{"content":"hello"}`, string(msg.payload))

	_, err = d.Next()
	assert.Equal(t, io.EOF, err, "expected io.EOF at end")
}

func TestEventStreamDecodeMultiple(t *testing.T) {
	f1 := esFrame(esStringHeader(":event-type", "assistantResponseEvent"), []byte(`{"content":"a"}`))
	f2 := esFrame(esStringHeader(":event-type", "assistantResponseEvent"), []byte(`{"content":"b"}`))
	d := newEventStreamDecoder(bytes.NewReader(append(f1, f2...)))

	for _, want := range []string{`{"content":"a"}`, `{"content":"b"}`} {
		msg, err := d.Next()
		require.NoError(t, err)
		assert.Equal(t, want, string(msg.payload))
	}
	_, err := d.Next()
	assert.Equal(t, io.EOF, err)
}

func TestEventStreamSkipsNonStringHeaders(t *testing.T) {
	// An int32 header must be consumed (not stored) without breaking offsets.
	headers := append(esInt32Header("count", 42), esStringHeader(":event-type", "toolUseEvent")...)
	frame := esFrame(headers, []byte(`{}`))

	msg, err := newEventStreamDecoder(bytes.NewReader(frame)).Next()
	require.NoError(t, err)
	assert.Equal(t, "toolUseEvent", msg.eventType())
	_, ok := msg.headers["count"]
	assert.False(t, ok, "int32 header should not be stored")
}

func TestEventStreamPreludeCRCMismatch(t *testing.T) {
	frame := esFrame(esStringHeader(":event-type", "x"), []byte(`{}`))
	frame[8] ^= 0xff // corrupt prelude CRC
	// smithy-go surfaces any CRC failure (prelude or message) as a single
	// "message checksum mismatch" error, so we only assert detection here.
	_, err := newEventStreamDecoder(bytes.NewReader(frame)).Next()
	assert.ErrorContains(t, err, "checksum")
}

func TestEventStreamMessageCRCMismatch(t *testing.T) {
	frame := esFrame(esStringHeader(":event-type", "x"), []byte(`{"a":1}`))
	frame[len(frame)-1] ^= 0xff // corrupt trailing message CRC
	_, err := newEventStreamDecoder(bytes.NewReader(frame)).Next()
	assert.ErrorContains(t, err, "checksum")
}

func TestEventStreamEmpty(t *testing.T) {
	_, err := newEventStreamDecoder(bytes.NewReader(nil)).Next()
	assert.Equal(t, io.EOF, err, "expected io.EOF on empty stream")
}
