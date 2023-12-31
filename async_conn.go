package ws

import (
	"errors"
	"io"
)

// upstream stdin
type Stream interface {
	Send([]byte) error
	Close(error)
}

// downstream stdout
type Handler interface {
	Received([]byte)
	Disconnected(error)
}

// AsyncConn
// Makes copy of the payload before passing it downstream
type AsyncConn struct {
	stream            Stream
	handler           Handler
	permessageDeflate bool         // connection option
	ms                messageState // fragmented message state
	fs                frameState   // partial frame parsing state
}

type messageState struct {
	payload           []byte
	opcode            OpCode
	prevFrameFragment Fragment
	compressed        bool
}

func (ms *messageState) reset() {
	ms.payload = nil
	ms.opcode = None
	ms.prevFrameFragment = fragSingle
	ms.compressed = false
}

func (ms *messageState) add(frame Frame) {
	if frame.first() {
		ms.compressed = frame.rsv1()
		ms.opcode = frame.opcode
	}
	// payload is always own copy
	ms.payload = append(ms.payload, frame.payload...)
	ms.prevFrameFragment = frame.fragment()
}

type frameState struct {
	pending  []byte // unprocessed part of the received buffer
	recvMore int    // how much bytes is needed for frame parsing to advance
}

func (fs *frameState) received(buf []byte) []byte {
	if len(buf) < fs.recvMore {
		fs.pending = append(fs.pending, buf...)
		fs.recvMore -= len(buf)
		return nil // nothing to process waiting for more
	}
	if len(fs.pending) > 0 {
		fs.recvMore = 0
		return append(fs.pending, buf...) // process pending + buf
	}
	return buf // nothing pending process buf
}

func (fs *frameState) unprocessed(buf []byte, recvMore int) {
	fs.pending = append(fs.pending, buf...)
	fs.recvMore = recvMore
}

func (fs *frameState) reset() {
	fs.pending = nil
	fs.recvMore = 0
}

func (c *AsyncConn) Received(buf []byte) {
	if b := c.fs.received(buf); b != nil {
		if err := c.readFrames(b); err != nil {
			c.stream.Close(err)
		}
	}
}

func (c *AsyncConn) readFrames(buf []byte) error {
	bbr := &bufferBytesReader{buf: buf}
	rdr := FrameReader{rd: bbr}

	for {
		frame, err := rdr.Read()
		if err != nil {
			if err == io.EOF {
				// reached end of the buffer cleanly
				// everything in buffer processed
				return nil
			}
			var erm *ErrReadMore
			if errors.As(err, &erm) {
				c.fs.unprocessed(bbr.pending(), erm.Bytes)
				return nil
			}
			return err
		}
		c.fs.reset()
		if frame.isControl() {
			c.handleControl(frame)
			continue
		}
		if err := verifyFrame(frame, c.ms.prevFrameFragment, c.permessageDeflate); err != nil {
			return err
		}
		c.ms.add(frame)

		if frame.fin {
			payload := c.ms.payload
			if c.ms.compressed {
				payload, err = Decompress(payload)
				if err != nil {
					return err
				}
			}
			if err := verifyMessage(c.ms.opcode, payload); err != nil {
				return err
			}
			c.handler.Received(payload) // send message downstream
			c.ms.reset()
		}
	}
}

func (c *AsyncConn) handleControl(frame Frame) {
	switch frame.opcode {
	case Ping:
		c.send(Pong, toOwnCopy(frame.payload))
	case Pong:
		// nothing to do on pong
		return
	case Close:
		c.send(Close, toOwnCopy(frame.payload))
	default:
		panic("not a control frame")
	}
}

func toOwnCopy(payload []byte) []byte {
	if len(payload) == 0 {
		return nil
	}
	dst := make([]byte, len(payload))
	copy(dst, payload)
	return dst
}

func (c *AsyncConn) send(opcode OpCode, payload []byte) {
	// TODO support buffers send into upstream aio
	buffers, err := encodeFrame(opcode, payload, c.permessageDeflate)
	if err != nil {
		// TODO
		return
	}
	nn := 0
	for _, b := range buffers {
		nn += len(b)
	}
	buf := make([]byte, nn)
	nn = 0
	for _, b := range buffers {
		copy(buf[nn:], b)
		nn += len(b)
	}
	c.stream.Send(buf)
}

func (c *AsyncConn) Disconnected(err error) {

}
