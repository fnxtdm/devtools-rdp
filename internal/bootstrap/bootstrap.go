package bootstrap

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"devtools-rdp/internal/config"
	"devtools-rdp/internal/rdp"
	"devtools-rdp/internal/tunnel"
)

type Bootstrapper struct {
	rdp    *rdp.Client
	tun    *tunnel.Server
	target config.Target
	agent  []byte
	logger *log.Logger

	ready     chan struct{}
	readyOnce sync.Once
	mu        sync.Mutex
	buf       []byte
	loaderOK  bool
	parser    *tunnel.Parser
}

func New(rdpClient *rdp.Client, tun *tunnel.Server, target config.Target, agent []byte, logger *log.Logger) *Bootstrapper {
	b := &Bootstrapper{
		rdp:    rdpClient,
		tun:    tun,
		target: target,
		agent:  agent,
		logger: logger,
		ready:  make(chan struct{}),
		parser: &tunnel.Parser{},
	}
	rdpClient.SetChannelHandler(b.handleChannel)
	return b
}

// handleChannel multiplexes the bootstrapping handshake and the framed tunnel.
func (b *Bootstrapper) handleChannel(data []byte) {
	b.mu.Lock()
	if !b.loaderOK {
		b.buf = append(b.buf, data...)
		if bytes.HasPrefix(b.buf, []byte("READY")) {
			b.loaderOK = true
			b.buf = nil
			b.mu.Unlock()
			b.readyOnce.Do(func() { close(b.ready) })
			return
		}
		if len(b.buf) > 64 {
			b.buf = b.buf[:0]
		}
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()

	frames, err := b.parser.Feed(data)
	if err != nil {
		b.logger.Printf("channel frame error: %v", err)
		return
	}
	for _, f := range frames {
		b.tun.HandleFrame(f)
	}
}

func (b *Bootstrapper) Run(ctx context.Context) error {
	command, err := LoaderCommand(b.target.ChannelName)
	if err != nil {
		return err
	}
	b.logger.Printf("loader command is %d characters", len(command))

	// A freshly created RDP session spends a while on the welcome screen.
	b.logger.Printf("waiting for the remote desktop")
	time.Sleep(10 * time.Second)

	for attempt := 1; attempt <= 3; attempt++ {
		if attempt == 3 {
			b.logger.Printf("trying to unlock the desktop")
			b.unlock()
		}
		b.openPowerShell()
		b.logger.Printf("sending loader (attempt %d)", attempt)
		b.rdp.TypeText(command, time.Duration(b.target.RDP.TypeDelayMS)*time.Millisecond)
		time.Sleep(500 * time.Millisecond)
		b.rdp.Tap("enter")

		select {
		case <-b.ready:
			b.logger.Printf("loader is ready, sending agent (%d bytes)", len(b.agent))
			return b.sendAgent()
		case <-time.After(25 * time.Second):
			b.logger.Printf("loader did not report in")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("loader did not start; check that the RDP session is logged in")
}

func (b *Bootstrapper) sendAgent() error {
	agentCfg, _ := json.Marshal(map[string]interface{}{"channel": b.target.ChannelName})
	msg := make([]byte, 0, 8+len(agentCfg)+len(b.agent))
	msg = appendBlob(msg, agentCfg)
	msg = appendBlob(msg, b.agent)
	if err := b.rdp.SendChannel(msg); err != nil {
		return fmt.Errorf("send agent: %w", err)
	}
	return nil
}

func appendBlob(dst, blob []byte) []byte {
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(blob)))
	dst = append(dst, hdr[:]...)
	return append(dst, blob...)
}

func (b *Bootstrapper) openPowerShell() {
	b.rdp.Hotkey([]string{"lwin"}, "r")
	time.Sleep(1800 * time.Millisecond)

	b.rdp.Hotkey([]string{"ctrl"}, "a")
	time.Sleep(200 * time.Millisecond)
	b.rdp.TypeText("powershell", 40*time.Millisecond)
	time.Sleep(600 * time.Millisecond)
	b.rdp.Tap("enter")
	time.Sleep(6 * time.Second)

	b.rdp.Hotkey([]string{"ctrl"}, "c")
	time.Sleep(400 * time.Millisecond)
	b.rdp.Tap("enter")
	time.Sleep(400 * time.Millisecond)
}

func (b *Bootstrapper) unlock() {
	b.rdp.SendSAS()
	time.Sleep(2500 * time.Millisecond)
	b.rdp.Tap("esc")
	time.Sleep(1500 * time.Millisecond)
	b.rdp.TypeText(b.target.RDP.Password, 50*time.Millisecond)
	time.Sleep(400 * time.Millisecond)
	b.rdp.Tap("enter")
	time.Sleep(8 * time.Second)
}
