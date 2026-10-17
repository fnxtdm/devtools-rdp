package tunnel

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

type stream struct {
	id      uint32
	conn    net.Conn
	opened  chan struct{}
	out     chan []byte
	err     error
	once    sync.Once
	outOnce sync.Once
	closed  bool
}

func (s *stream) markOpen(err error) {
	s.once.Do(func() {
		s.err = err
		close(s.opened)
	})
}

type Server struct {
	forwards []Forward
	send     func([]byte) error
	log      *log.Logger

	lns     []net.Listener
	mu      sync.Mutex
	streams map[uint32]*stream
	nextID  uint32

	helloOnce sync.Once
	helloCh   chan []byte
}

// Forward maps a local listening address to an address the agent dials remotely.
type Forward struct {
	Listen string
	Remote string
}

func New(send func([]byte) error, logger *log.Logger) *Server {
	return &Server{
		send:    send,
		log:     logger,
		streams: make(map[uint32]*stream),
		nextID:  1,
		helloCh:  make(chan []byte, 1),
	}
}

func (s *Server) AddForward(listenAddr, remote string) {
	s.forwards = append(s.forwards, Forward{Listen: listenAddr, Remote: remote})
}

func (s *Server) Start() error {
	s.mu.Lock()
	pending := s.forwards
	s.forwards = nil
	s.mu.Unlock()
	for _, f := range pending {
		if err := s.Listen(f); err != nil {
			s.Close()
			return err
		}
	}
	return nil
}

// Listen starts one forwarding listener. Safe to call while the tunnel is live.
func (s *Server) Listen(f Forward) error {
	ln, err := net.Listen("tcp", f.Listen)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.lns = append(s.lns, ln)
	s.forwards = append(s.forwards, f)
	s.mu.Unlock()
	s.log.Printf("forwarding %s -> %s", ln.Addr(), f.Remote)
	go s.acceptLoop(ln, f.Remote)
	return nil
}

// Forwards returns the active mappings.
func (s *Server) Forwards() []Forward {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Forward(nil), s.forwards...)
}

func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.lns) == 0 {
		return nil
	}
	return s.lns[0].Addr()
}

func (s *Server) Close() {
	s.mu.Lock()
	lns := s.lns
	s.lns = nil
	s.forwards = nil
	streams := s.streams
	s.streams = map[uint32]*stream{}
	s.mu.Unlock()
	for _, ln := range lns {
		ln.Close()
	}
	for _, st := range streams {
		st.conn.Close()
	}
}

func (s *Server) WaitHello(ctx context.Context) ([]byte, error) {
	select {
	case h := <-s.helloCh:
		return h, nil
	case <-ctx.Done():
		return nil, errors.New("timed out waiting for remote agent")
	}
}

func (s *Server) acceptLoop(ln net.Listener, remote string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		id := s.nextID
		s.nextID++
		st := &stream{id: id, conn: conn, opened: make(chan struct{}), out: make(chan []byte, 256)}
		go s.pumpRemote(st)
		s.streams[id] = st
		s.mu.Unlock()

		if err := s.send(Encode(MsgOpen, id, []byte(remote))); err != nil {
			s.log.Printf("stream %d: send OPEN: %v", id, err)
			conn.Close()
			s.remove(id)
			continue
		}
		go s.pumpLocal(st)
	}
}

func (s *Server) pumpLocal(st *stream) {
	defer s.remove(st.id)
	select {
	case <-st.opened:
	case <-time.After(15 * time.Second):
		st.conn.Close()
		return
	}
	if st.err != nil {
		st.conn.Close()
		return
	}
	buf := make([]byte, 16*1024)
	for {
		n, err := st.conn.Read(buf)
		if n > 0 {
			if sendErr := s.send(Encode(MsgData, st.id, buf[:n])); sendErr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	s.send(Encode(MsgClose, st.id, nil))
	st.conn.Close()
}

func (s *Server) HandleFrame(f Frame) {
	switch f.Type {
	case MsgHello:
		s.helloOnce.Do(func() { s.helloCh <- f.Payload })
		return
	case MsgLog:
		s.log.Printf("agent: %s", string(f.Payload))
		return
	}

	s.mu.Lock()
	st := s.streams[f.StreamID]
	s.mu.Unlock()
	if st == nil {
		return
	}

	switch f.Type {
	case MsgOpenOK:
		st.markOpen(nil)
	case MsgOpenErr:
		st.markOpen(fmt.Errorf("%s", string(f.Payload)))
		st.conn.Close()
	case MsgData:
		select {
		case st.out <- f.Payload:
		default:
			st.conn.Close()
			s.remove(st.id)
		}
	case MsgClose:
		st.conn.Close()
		s.remove(st.id)
	}
}

func (s *Server) pumpRemote(st *stream) {
	for data := range st.out {
		st.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
		if _, err := st.conn.Write(data); err != nil {
			st.conn.Close()
			return
		}
	}
}

func (s *Server) remove(id uint32) {
	s.mu.Lock()
	st := s.streams[id]
	delete(s.streams, id)
	s.mu.Unlock()
	if st != nil {
		st.outOnce.Do(func() { close(st.out) })
	}
}
