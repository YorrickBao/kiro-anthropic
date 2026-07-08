package main

import (
	"bufio"
	"io"

	"github.com/aws/smithy-go/eventstream"
)

// This file adapts AWS's smithy-go event stream decoder to the small,
// protocol-agnostic view the rest of the code needs. The wire format is AWS's
// "vnd.amazon.eventstream" binary framing used by CodeWhisperer/Kiro streaming
// responses; smithy-go owns the framing, CRC validation and header decoding, so
// this file only translates its Message into an eventMessage.

// eventMessage is one decoded event stream frame, reduced to the header values
// and payload the translation layer consumes.
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
	r   io.Reader
	dec *eventstream.Decoder
}

func newEventStreamDecoder(r io.Reader) *eventStreamDecoder {
	// A buffered reader lets smithy read header names cheaply (it prefers
	// io.ByteReader when available) and carries any read-ahead between the
	// per-message Decode calls.
	return &eventStreamDecoder{
		r:   bufio.NewReaderSize(r, 32*1024),
		dec: eventstream.NewDecoder(),
	}
}

// Next reads and returns the next message. It returns io.EOF when the stream is
// cleanly exhausted. A nil payload buffer makes smithy allocate a fresh payload
// slice per call, so the returned payload is safe to retain.
func (d *eventStreamDecoder) Next() (*eventMessage, error) {
	msg, err := d.dec.Decode(d.r, nil)
	if err != nil {
		return nil, err // io.EOF passes through cleanly at end of stream
	}

	// Keep only string/bytes header values (the ":*" metadata headers Kiro
	// sends). Numeric/timestamp/uuid headers are not used by the mapping and
	// are dropped, matching the previous decoder's behaviour.
	headers := make(map[string]string, len(msg.Headers))
	for _, h := range msg.Headers {
		switch v := h.Value.Get().(type) {
		case string:
			headers[h.Name] = v
		case []byte:
			headers[h.Name] = string(v)
		}
	}

	return &eventMessage{headers: headers, payload: msg.Payload}, nil
}
