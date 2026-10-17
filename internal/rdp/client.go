package rdp

import (
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/tomatome/grdp/core"
	"github.com/tomatome/grdp/glog"
	"github.com/tomatome/grdp/plugin"
	"github.com/tomatome/grdp/protocol/nla"
	"github.com/tomatome/grdp/protocol/pdu"
	"github.com/tomatome/grdp/protocol/sec"
	"github.com/tomatome/grdp/protocol/t125"
	"github.com/tomatome/grdp/protocol/tpkt"
	"github.com/tomatome/grdp/protocol/x224"

	"devtools-rdp/internal/config"
)

const optData = 0xC8000000 // INITIALIZED | ENCRYPT_RDP | PRI_HIGH

type chanTransport struct {
	name    string
	options uint32
	onData  func([]byte)
	sender  core.ChannelSender
}

func (t *chanTransport) GetType() (string, uint32) { return t.name, t.options }
func (t *chanTransport) Sender(s core.ChannelSender) {
	t.sender = s
}
func (t *chanTransport) Process(s []byte) {
	if t.onData != nil {
		t.onData(s)
	}
}

type Client struct {
	cfg     config.RDP
	channel string
	logger  *log.Logger

	pdu      *pdu.Client
	channels *plugin.Channels
	sendMu   sync.Mutex
	data     *chanTransport
}

func New(cfg config.RDP, channel string, logger *log.Logger) *Client {
	glog.SetLogger(logger)
	glog.SetLevel(glog.WARN)
	return &Client{cfg: cfg, channel: channel, logger: logger}
}

func (c *Client) Connect() error {
	addr := net.JoinHostPort(c.cfg.Host, fmt.Sprintf("%d", c.cfg.Port))
	conn, err := net.DialTimeout("tcp", addr, 15*time.Second)
	if err != nil {
		return err
	}

	tp := tpkt.New(core.NewSocketLayer(conn), nla.NewNTLMv2(c.cfg.Domain, c.cfg.Username, c.cfg.Password))
	x := x224.New(tp)
	mcs := t125.NewMCSClient(x)
	sec := sec.NewClient(mcs)
	p := pdu.NewClient(sec)
	channels := plugin.NewChannels(sec)

	channels.SetChannelSender(sec)
	tp.SetFastPathListener(sec)
	sec.SetFastPathListener(p)
	sec.SetChannelSender(mcs)

	mcs.SetClientDesktop(uint16(c.cfg.Width), uint16(c.cfg.Height))
	mcs.SetClientStaticChannel(c.channel, optData)

	sec.SetUser(c.cfg.Username)
	sec.SetPwd(c.cfg.Password)
	sec.SetDomain(c.cfg.Domain)

	c.pdu = p
	c.channels = channels
	c.data = &chanTransport{name: c.channel, options: optData}
	channels.Register(c.data)

	x.SetRequestedProtocol(x224.PROTOCOL_SSL | x224.PROTOCOL_HYBRID)
	if err := x.Connect(); err != nil {
		return err
	}
	time.Sleep(2 * time.Second)
	return nil
}

// SetChannelHandler installs the callback for data-channel payloads.
func (c *Client) SetChannelHandler(fn func([]byte)) {
	c.data.onData = fn
}

// SendChannel sends a payload on the CDP channel (chunked by the RDP stack).
func (c *Client) SendChannel(data []byte) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	_, err := c.channels.SendToChannel(c.channel, data)
	return err
}

// ---- input -------------------------------------------------------------

func (c *Client) key(sc uint16, down, extended bool) {
	e := &pdu.ScancodeKeyEvent{KeyCode: sc}
	if !down {
		e.KeyboardFlags |= pdu.KBDFLAGS_RELEASE
	}
	if extended {
		e.KeyboardFlags |= pdu.KBDFLAGS_EXTENDED
	}
	c.pdu.SendInputEvents(pdu.INPUT_EVENT_SCANCODE, []pdu.InputEventsInterface{e})
}

func (c *Client) unicode(ch rune, down bool) {
	e := &pdu.UnicodeKeyEvent{Unicode: uint16(ch)}
	if !down {
		e.KeyboardFlags |= pdu.KBDFLAGS_RELEASE
	}
	c.pdu.SendInputEvents(pdu.INPUT_EVENT_UNICODE, []pdu.InputEventsInterface{e})
}

var scan = map[string]uint16{
	"esc": 0x01, "enter": 0x1C, "ctrl": 0x1D, "alt": 0x38,
	"space": 0x39, "lwin": 0x5B, "r": 0x13, "a": 0x1E,
	"c": 0x2E, "delete": 0x53,
}

var extended = map[string]bool{"lwin": true, "delete": true}

func (c *Client) Tap(name string) {
	sc := scan[name]
	c.key(sc, true, extended[name])
	time.Sleep(30 * time.Millisecond)
	c.key(sc, false, extended[name])
	time.Sleep(30 * time.Millisecond)
}

func (c *Client) Hotkey(mods []string, keyName string) {
	for _, m := range mods {
		c.key(scan[m], true, extended[m])
		time.Sleep(20 * time.Millisecond)
	}
	c.key(scan[keyName], true, extended[keyName])
	time.Sleep(20 * time.Millisecond)
	c.key(scan[keyName], false, extended[keyName])
	for i := len(mods) - 1; i >= 0; i-- {
		time.Sleep(20 * time.Millisecond)
		c.key(scan[mods[i]], false, extended[mods[i]])
	}
	if contains(mods, "lwin") {
		time.Sleep(30 * time.Millisecond)
		c.key(scan["lwin"], false, true)
	}
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func (c *Client) TypeText(s string, delay time.Duration) {
	for _, ch := range s {
		c.unicode(ch, true)
		c.unicode(ch, false)
		time.Sleep(delay)
	}
}

func (c *Client) SendSAS() {
	c.key(scan["ctrl"], true, false)
	c.key(scan["alt"], true, false)
	c.key(scan["delete"], true, true)
	c.key(scan["delete"], false, true)
	c.key(scan["alt"], false, false)
	c.key(scan["ctrl"], false, false)
}
