package tunnel

import (
	"encoding/binary"
	"fmt"
)

const (
	MsgOpen    byte = 1
	MsgOpenOK  byte = 2
	MsgOpenErr byte = 3
	MsgData    byte = 4
	MsgClose   byte = 5
	MsgHello   byte = 6
	MsgLog     byte = 7
)

const HeaderSize = 9

type Frame struct {
	Type     byte
	StreamID uint32
	Payload  []byte
}

func Encode(msgType byte, streamID uint32, payload []byte) []byte {
	out := make([]byte, HeaderSize+len(payload))
	out[0] = msgType
	binary.LittleEndian.PutUint32(out[1:], streamID)
	binary.LittleEndian.PutUint32(out[5:], uint32(len(payload)))
	copy(out[HeaderSize:], payload)
	return out
}

// Parser reassembles frames from a byte stream.
type Parser struct {
	buf []byte
}

func (p *Parser) Feed(data []byte) ([]Frame, error) {
	p.buf = append(p.buf, data...)
	var frames []Frame
	for len(p.buf) >= HeaderSize {
		typ := p.buf[0]
		id := binary.LittleEndian.Uint32(p.buf[1:])
		n := binary.LittleEndian.Uint32(p.buf[5:])
		if n > 64*1024*1024 {
			return frames, fmt.Errorf("frame too large: %d", n)
		}
		if uint32(len(p.buf)) < uint32(HeaderSize)+n {
			break
		}
		payload := make([]byte, n)
		copy(payload, p.buf[HeaderSize:HeaderSize+int(n)])
		p.buf = p.buf[HeaderSize+int(n):]
		frames = append(frames, Frame{Type: typ, StreamID: id, Payload: payload})
	}
	return frames, nil
}
