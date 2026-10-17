//go:build windows

// devtools-rdp-agent runs inside the Windows RDP session. It opens the static
// virtual channel created by the client and bridges framed messages to TCP
// sockets on the local machine, so any port can be reached from the client.
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	msgOpen    = 1
	msgOpenOK  = 2
	msgOpenErr = 3
	msgData    = 4
	msgClose   = 5
	msgHello   = 6
	msgLog     = 7
)

type agentConfig struct {
	Channel string `json:"channel"`
}

var (
	wtsapi    = syscall.NewLazyDLL("wtsapi32.dll")
	procOpen  = wtsapi.NewProc("WTSVirtualChannelOpen")
	procRead  = wtsapi.NewProc("WTSVirtualChannelRead")
	procWrite = wtsapi.NewProc("WTSVirtualChannelWrite")
	procClose = wtsapi.NewProc("WTSVirtualChannelClose")

	channel syscall.Handle
	writeMu sync.Mutex
	connMu  sync.Mutex
	conns   = map[uint32]net.Conn{}

	inMu  sync.Mutex
	inBuf []byte
)

func openChannel(name string) (syscall.Handle, error) {
	p, err := syscall.BytePtrFromString(name)
	if err != nil {
		return 0, err
	}
	r, _, e := procOpen.Call(0, uintptr(^uint32(0)), uintptr(unsafe.Pointer(p)))
	if r == 0 {
		return 0, e
	}
	return syscall.Handle(r), nil
}

func readChannel(buf []byte) (int, error) {
	var n uint32
	r, _, e := procRead.Call(uintptr(channel), 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		return 0, e
	}
	return int(n), nil
}

func writeChannel(data []byte) error {
	writeMu.Lock()
	defer writeMu.Unlock()
	for len(data) > 0 {
		chunk := data
		if len(chunk) > 1024 {
			chunk = chunk[:1024]
		}
		var n uint32
		r, _, e := procWrite.Call(uintptr(channel), uintptr(unsafe.Pointer(&chunk[0])),
			uintptr(len(chunk)), uintptr(unsafe.Pointer(&n)))
		if r == 0 {
			return e
		}
		if n == 0 {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		data = data[n:]
	}
	return nil
}

func sendFrame(typ byte, id uint32, payload []byte) error {
	out := make([]byte, 9+len(payload))
	out[0] = typ
	binary.LittleEndian.PutUint32(out[1:], id)
	binary.LittleEndian.PutUint32(out[5:], uint32(len(payload)))
	copy(out[9:], payload)
	return writeChannel(out)
}

func logf(msg string) {
	_ = sendFrame(msgLog, 0, []byte(msg))
}

func main() {
	cfgPath := flag.String("config", "", "path to agent json config")
	flag.Parse()

	cfg := agentConfig{Channel: "rdpdt"}
	if *cfgPath != "" {
		if raw, err := os.ReadFile(*cfgPath); err == nil {
			_ = json.Unmarshal(raw, &cfg)
		}
	}

	logFile, _ := os.OpenFile(filepath.Join(os.TempDir(), "devtools-rdp-agent.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if logFile != nil {
		defer logFile.Close()
		log.SetOutput(io.MultiWriter(logFile))
	}
	log.Printf("agent starting, channel=%s", cfg.Channel)

	h, err := openChannel(cfg.Channel)
	if err != nil {
		log.Printf("WTSVirtualChannelOpen(%s): %v", cfg.Channel, err)
		return
	}
	channel = h
	defer procClose.Call(uintptr(channel))

	hello, _ := json.Marshal(map[string]interface{}{"agent": "devtools-rdp", "version": 2})
	_ = sendFrame(msgHello, 0, hello)
	log.Printf("hello sent")

	go readerLoop()
	select {}
}

func readerLoop() {
	buf := make([]byte, 32768)
	fails := 0
	for {
		n, err := readChannel(buf)
		if err != nil {
			fails++
			errno, _ := err.(syscall.Errno)
			if fails == 1 || fails%2000 == 0 {
				logf(fmt.Sprintf("read error %d (consecutive %d)", uint32(errno), fails))
			}
			if fails > 6000 {
				log.Printf("giving up on channel reads")
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		fails = 0
		if n <= 0 {
			continue
		}
		inMu.Lock()
		inBuf = append(inBuf, buf[:n]...)
		for len(inBuf) >= 9 {
			typ := inBuf[0]
			id := binary.LittleEndian.Uint32(inBuf[1:])
			l := int(binary.LittleEndian.Uint32(inBuf[5:]))
			if l < 0 || l > 64*1024*1024 {
				inBuf = nil
				break
			}
			if len(inBuf) < 9+l {
				break
			}
			payload := make([]byte, l)
			copy(payload, inBuf[9:9+l])
			inBuf = inBuf[9+l:]
			inMu.Unlock()
			handleFrame(typ, id, payload)
			inMu.Lock()
		}
		inMu.Unlock()
	}
}

func handleFrame(typ byte, id uint32, payload []byte) {
	switch typ {
	case msgOpen:
		target := string(payload)
		conn, err := net.DialTimeout("tcp", target, 5*time.Second)
		if err != nil {
			_ = sendFrame(msgOpenErr, id, []byte(err.Error()))
			return
		}
		connMu.Lock()
		conns[id] = conn
		connMu.Unlock()
		_ = sendFrame(msgOpenOK, id, nil)
		go pump(id, conn)
	case msgData:
		connMu.Lock()
		conn := conns[id]
		connMu.Unlock()
		if conn != nil {
			if _, err := conn.Write(payload); err != nil {
				closeConn(id)
			}
		}
	case msgClose:
		closeConn(id)
	}
}

func pump(id uint32, conn net.Conn) {
	buf := make([]byte, 16*1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if sendErr := sendFrame(msgData, id, buf[:n]); sendErr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	closeConn(id)
}

func closeConn(id uint32) {
	connMu.Lock()
	conn := conns[id]
	delete(conns, id)
	connMu.Unlock()
	if conn != nil {
		conn.Close()
		_ = sendFrame(msgClose, id, nil)
	}
}
