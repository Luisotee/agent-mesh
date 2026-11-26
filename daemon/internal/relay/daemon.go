package relay

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	relayv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/relay/v1"
)

var (
	errControlAuthFailed = errors.New("relay rejected daemon token")
	errControlReplaced   = errors.New("relay control connection replaced by another session")
)

// ConnectorConfig configures a daemon's outbound connection to a relay Server.
type ConnectorConfig struct {
	RelayURL      string
	Secret        string
	HostID        string
	HostName      string
	DaemonVersion string
	Logger        *slog.Logger
	HTTPClient    *http.Client
	// HeartbeatInterval is how often Ping frames are sent on the control connection (default 25s).
	HeartbeatInterval time.Duration
	// HeartbeatTimeout is how long to wait for a Pong ack (default 10s).
	HeartbeatTimeout time.Duration
	// ClientTokens, when set, lists the client tokens the relay may accept for this host (by hash), read at
	// every registration and after every TokensChanged. The relay then accepts no other token, the host-wide
	// one included. Nil keeps the host-wide token (ClientToken).
	ClientTokens func() []*relayv1.ClientToken
}

// Connector maintains the daemon's outbound control connection to the relay and yields dialed-back
// streams on Listener() so the daemon's TLS http.Server can serve them directly.
type Connector struct {
	cfg      ConnectorConfig
	log      *slog.Logger
	hc       *http.Client
	wsBase   string
	listener *chanListener
	// tokensChanged holds one pending "send the relay the client tokens again".
	tokensChanged chan struct{}

	online atomic.Bool
}

// NewConnector validates cfg and creates a Connector.
func NewConnector(cfg ConnectorConfig) (*Connector, error) {
	if strings.TrimSpace(cfg.RelayURL) == "" {
		return nil, errors.New("relay URL is required")
	}
	if strings.TrimSpace(cfg.Secret) == "" {
		return nil, errors.New("relay secret is required")
	}
	if strings.TrimSpace(cfg.HostID) == "" {
		return nil, errors.New("host_id is required")
	}
	wsBase, err := toWSBase(cfg.RelayURL)
	if err != nil {
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{
			Transport: &http.Transport{
				DialContext: DialTCPNoDelay,
			},
		}
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = 25 * time.Second
	}
	if cfg.HeartbeatTimeout == 0 {
		cfg.HeartbeatTimeout = 10 * time.Second
	}
	return &Connector{
		cfg:           cfg,
		log:           cfg.Logger,
		hc:            hc,
		wsBase:        wsBase,
		listener:      newChanListener(wsAddr("relay:" + cfg.HostID)),
		tokensChanged: make(chan struct{}, 1),
	}, nil
}

// Listener returns the net.Listener that accepts dialed-back streams from the relay.
func (c *Connector) Listener() net.Listener { return c.listener }

// TokensChanged tells the connector the set of client tokens changed: the live control connection sends it again
// (a later registration reads it anyway). It never blocks.
func (c *Connector) TokensChanged() {
	select {
	case c.tokensChanged <- struct{}{}:
	default:
	}
}

func (c *Connector) clientTokens() []*relayv1.ClientToken {
	list := c.cfg.ClientTokens()
	if len(list) > MaxClientTokens {
		c.log.Error("too many client relay tokens; the relay gets the first ones", "tokens", len(list), "max", MaxClientTokens)
		list = list[:MaxClientTokens]
	}
	return list
}

// Online reports whether the control connection is registered and healthy.
func (c *Connector) Online() bool { return c.online.Load() }

// Run keeps the control connection open with exponential backoff until ctx is canceled, authentication
// permanently fails (HTTP 401/403), or a newer control session replaces this one (close code 4001).
func (c *Connector) Run(ctx context.Context) error {
	defer c.listener.Close()
	backoff := 100 * time.Millisecond
	const maxBackoff = 30 * time.Second

	for {
		acked, err := c.connectOnce(ctx)
		c.online.Store(false)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errControlAuthFailed) {
			c.log.Error("relay authentication failed; stopping reconnect", "err", err)
			return err
		}
		if errors.Is(err, errControlReplaced) {
			c.log.Info("relay control connection replaced by newer session")
			return nil
		}
		if acked {
			backoff = 100 * time.Millisecond
		}
		c.log.Warn("relay control disconnected; retrying", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if !acked {
			backoff = min(backoff*2, maxBackoff)
		}
	}
}

func (c *Connector) connectOnce(ctx context.Context) (heartbeatAcked bool, err error) {
	hdr := http.Header{"Authorization": {"Bearer " + c.cfg.Secret}}
	ws, resp, err := websocket.Dial(ctx, c.wsBase+"/v1/daemon/control", &websocket.DialOptions{
		HTTPClient: c.hc,
		HTTPHeader: hdr,
	})
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return false, errControlAuthFailed
		}
		return false, err
	}
	ws.SetReadLimit(64 * 1024)
	defer ws.CloseNow()

	reg := &relayv1.Register{
		HostId:        c.cfg.HostID,
		HostName:      c.cfg.HostName,
		DaemonVersion: c.cfg.DaemonVersion,
	}
	if c.cfg.ClientTokens != nil {
		// Drain first: a change after this read rings again and is sent once registered.
		select {
		case <-c.tokensChanged:
		default:
		}
		reg.DeviceTokens = true
		reg.ClientTokens = c.clientTokens()
	}
	if err := writeControlFrame(ctx, ws, &relayv1.ControlFrame{
		Frame: &relayv1.ControlFrame_Register{Register: reg},
	}); err != nil {
		return false, err
	}

	hctx, hcancel := context.WithTimeout(ctx, 10*time.Second)
	ack, err := readControlFrame(hctx, ws)
	hcancel()
	if err != nil {
		if websocket.CloseStatus(err) == CloseReplaced {
			return false, errControlReplaced
		}
		return false, err
	}
	if ack.GetRegistered() == nil {
		return false, errors.New("expected Registered frame from relay")
	}

	c.online.Store(true)
	c.log.Info("connected to relay", "url", c.cfg.RelayURL, "host", c.cfg.HostID)

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var writeMu sync.Mutex
	sendPing := func(seq int64) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return writeControlFrame(connCtx, ws, &relayv1.ControlFrame{
			Frame: &relayv1.ControlFrame_Ping{Ping: &relayv1.Ping{Seq: seq}},
		})
	}

	if c.cfg.ClientTokens != nil {
		go func() {
			for {
				select {
				case <-connCtx.Done():
					return
				case <-c.tokensChanged:
				}
				list := c.clientTokens()
				writeMu.Lock()
				err := writeControlFrame(connCtx, ws, &relayv1.ControlFrame{
					Frame: &relayv1.ControlFrame_ClientTokens{ClientTokens: &relayv1.ClientTokens{Tokens: list}},
				})
				writeMu.Unlock()
				if err != nil {
					// The connection is going; the next registration carries the set. Ring again for it.
					c.TokensChanged()
					return
				}
			}
		}()
	}

	pongCh := make(chan int64, 8)
	hbErr := make(chan error, 1)
	go func() {
		var seq int64 = 1
		sendAndAwait := func() error {
			s := seq
			seq++
			if err := sendPing(s); err != nil {
				return err
			}
			t := time.NewTimer(c.cfg.HeartbeatTimeout)
			defer t.Stop()
			for {
				select {
				case got := <-pongCh:
					if got == s {
						return nil
					}
				case <-t.C:
					return errors.New("relay heartbeat timed out")
				case <-connCtx.Done():
					return connCtx.Err()
				}
			}
		}
		// Immediate first heartbeat verifies bidirectional health and resets backoff.
		if err := sendAndAwait(); err != nil {
			hbErr <- err
			cancel()
			return
		}
		tick := time.NewTicker(c.cfg.HeartbeatInterval)
		defer tick.Stop()
		for {
			select {
			case <-connCtx.Done():
				return
			case <-tick.C:
				if err := sendAndAwait(); err != nil {
					hbErr <- err
					cancel()
					return
				}
			}
		}
	}()

	for {
		f, err := readControlFrame(connCtx, ws)
		if err != nil {
			select {
			case herr := <-hbErr:
				return heartbeatAcked, herr
			default:
			}
			if websocket.CloseStatus(err) == CloseReplaced {
				return heartbeatAcked, errControlReplaced
			}
			return heartbeatAcked, err
		}
		switch v := f.Frame.(type) {
		case *relayv1.ControlFrame_Pong:
			heartbeatAcked = true
			select {
			case pongCh <- v.Pong.Seq:
			default:
			}
		case *relayv1.ControlFrame_DialBack:
			ticket := v.DialBack.Ticket
			go c.handleDialBack(connCtx, ticket)
		}
	}
}

func (c *Connector) handleDialBack(ctx context.Context, ticket string) {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	hdr := http.Header{"Authorization": {"Bearer " + c.cfg.Secret}}
	u := c.wsBase + "/v1/daemon/stream?ticket=" + url.QueryEscape(ticket)
	ws, _, err := websocket.Dial(dctx, u, &websocket.DialOptions{
		HTTPClient: c.hc,
		HTTPHeader: hdr,
	})
	if err != nil {
		c.log.Warn("dial-back failed", "err", err)
		return
	}
	sc := NewWSConn(ws, wsAddr("relay-daemon"), wsAddr("relay-client"))
	if !c.listener.enqueue(sc) {
		_ = sc.Close()
	}
}

func toWSBase(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", errors.New("relay URL scheme must be http, https, ws, or wss")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// chanListener is a net.Listener fed by dialed-back relay connections.
type chanListener struct {
	addr      net.Addr
	conns     chan net.Conn
	closed    chan struct{}
	closeOnce sync.Once
}

func newChanListener(addr net.Addr) *chanListener {
	return &chanListener{
		addr:   addr,
		conns:  make(chan net.Conn, 32),
		closed: make(chan struct{}),
	}
}

func (l *chanListener) enqueue(c net.Conn) bool {
	select {
	case <-l.closed:
		return false
	case l.conns <- c:
		return true
	}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.closed)
		for {
			select {
			case c := <-l.conns:
				_ = c.Close()
			default:
				return
			}
		}
	})
	return nil
}

func (l *chanListener) Addr() net.Addr { return l.addr }
