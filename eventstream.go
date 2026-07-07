package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
)

// This file implements a decoder for the AWS "vnd.amazon.eventstream" binary
// framing used by CodeWhisperer/Kiro streaming responses.
//
// Message layout (all integers big-endian):
//
//	0                   4                   8                  12
//	+---------+---------+---------+---------+---------+---------+
//	| total length (u32)| headers length(u32)| prelude CRC(u32)|
//	+---------+---------+---------+---------+---------+---------+
//	|                     headers (headers length bytes)       |
//	+----------------------------------------------------------+
//	|          payload (total - headers - 16 bytes)            |
//	+----------------------------------------------------------+
//	|                     message CRC (u32)                    |
//	+----------------------------------------------------------+
//
// Header entry layout:
//
//	name_len (u8) | name | value_type (u8) | value...
type eventMessage struct {
	headers map[string]string
	payload []byte
}

// eventType returns the value of the ":event-type" header.
func (m *eventMessage) eventType() string { return m.headers[":event-type"] }

// messageType returns the value of the ":message-type" header ("event" or "exception").
func (m *eventMessage) messageType() string { return m.headers[":message-type"] }

// exceptionType returns the value of the ":exception-type" header, if any.
func (m *eventMessage) exceptionType() string { return m.headers[":exception-type"] }

// eventStreamDecoder reads framed messages from an underlying stream.
type eventStreamDecoder struct {
	r *bufio.Reader
}

func newEventStreamDecoder(r io.Reader) *eventStreamDecoder {
	return &eventStreamDecoder{r: bufio.NewReaderSize(r, 32*1024)}
}

// Next reads and returns the next message. It returns io.EOF when the stream
// is cleanly exhausted.
func (d *eventStreamDecoder) Next() (*eventMessage, error) {
	// Read the 12-byte prelude.
	var prelude [12]byte
	if _, err := io.ReadFull(d.r, prelude[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			return nil, io.EOF
		}
		return nil, err // io.EOF passes through cleanly here
	}

	totalLen := binary.BigEndian.Uint32(prelude[0:4])
	headersLen := binary.BigEndian.Uint32(prelude[4:8])
	preludeCRC := binary.BigEndian.Uint32(prelude[8:12])

	if got := crc32.ChecksumIEEE(prelude[0:8]); got != preludeCRC {
		return nil, fmt.Errorf("eventstream: prelude CRC mismatch (got %08x want %08x)", got, preludeCRC)
	}

	if totalLen < 16 || totalLen > 64*1024*1024 {
		return nil, fmt.Errorf("eventstream: implausible message length %d", totalLen)
	}
	if headersLen > totalLen-16 {
		return nil, fmt.Errorf("eventstream: headers length %d exceeds message", headersLen)
	}

	// Read the remainder: headers + payload + trailing CRC.
	rest := make([]byte, totalLen-12)
	if _, err := io.ReadFull(d.r, rest); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}

	// Validate the whole-message CRC (prelude + rest-without-trailing-crc).
	msgCRC := binary.BigEndian.Uint32(rest[len(rest)-4:])
	crc := crc32.NewIEEE()
	_, _ = crc.Write(prelude[:])
	_, _ = crc.Write(rest[:len(rest)-4])
	if got := crc.Sum32(); got != msgCRC {
		return nil, fmt.Errorf("eventstream: message CRC mismatch (got %08x want %08x)", got, msgCRC)
	}

	headerBytes := rest[:headersLen]
	payload := rest[headersLen : len(rest)-4]

	headers, err := parseEventHeaders(headerBytes)
	if err != nil {
		return nil, err
	}

	// Copy payload so callers may retain it after the buffer is reused.
	payloadCopy := make([]byte, len(payload))
	copy(payloadCopy, payload)

	return &eventMessage{headers: headers, payload: payloadCopy}, nil
}

// parseEventHeaders decodes the header section. Only string/bytes headers are
// stored as values; other types are skipped (their bytes are still consumed so
// the offset stays correct).
func parseEventHeaders(b []byte) (map[string]string, error) {
	headers := make(map[string]string)
	i := 0
	for i < len(b) {
		nameLen := int(b[i])
		i++
		if i+nameLen > len(b) {
			return nil, fmt.Errorf("eventstream: truncated header name")
		}
		name := string(b[i : i+nameLen])
		i += nameLen
		if i >= len(b) {
			return nil, fmt.Errorf("eventstream: missing header value type")
		}
		valueType := b[i]
		i++

		switch valueType {
		case 0: // bool true
			headers[name] = "true"
		case 1: // bool false
			headers[name] = "false"
		case 2: // int8
			i += 1
		case 3: // int16
			i += 2
		case 4: // int32
			i += 4
		case 5: // int64
			i += 8
		case 6, 7: // byte array / string
			if i+2 > len(b) {
				return nil, fmt.Errorf("eventstream: truncated header value length")
			}
			vlen := int(binary.BigEndian.Uint16(b[i:]))
			i += 2
			if i+vlen > len(b) {
				return nil, fmt.Errorf("eventstream: truncated header value")
			}
			headers[name] = string(b[i : i+vlen])
			i += vlen
		case 8: // timestamp (int64 epoch millis)
			i += 8
		case 9: // uuid
			i += 16
		default:
			return nil, fmt.Errorf("eventstream: unknown header value type %d", valueType)
		}
		if i > len(b) {
			return nil, fmt.Errorf("eventstream: header overran section")
		}
	}
	return headers, nil
}
