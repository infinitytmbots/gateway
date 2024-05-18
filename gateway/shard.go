package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spec-tacles/gateway/compression"
	"github.com/spec-tacles/gateway/stats"
	"github.com/spec-tacles/go/types"
)

// Shard represents a Gateway shard
type Shard struct {
	Gateway *types.GatewayBot
	Ping    time.Duration

	conn *Connection

	id            string
	opts          *ShardOptions
	limiter       Limiter
	packets       *sync.Pool
	lastHeartbeat time.Time
	resumeURL     string

	connMu sync.Mutex
	acks   chan struct{}
}

// NewShard creates a new Gateway shard
func NewShard(opts *ShardOptions) *Shard {
	opts.init()

	return &Shard{
		opts:    opts,
		limiter: NewDefaultLimiter(120, time.Minute),
		packets: &sync.Pool{
			New: func() interface{} {
				return new(types.ReceivePacket)
			},
		},
		id:   strconv.Itoa(opts.Identify.Shard[0]),
		acks: make(chan struct{}),
		resumeURL: "",
	}
}

// Open starts a new session. Any errors are fatal.
func (s *Shard) Open(ctx context.Context) (err error) {
	err = s.connect(ctx)
	for s.handleClose(err) {
		err = s.connect(ctx)
	}
	return
}

// connect runs a single websocket connection; errors may indicate the connection is recoverable
func (s *Shard) connect(ctx context.Context) (err error) {
	if s.Gateway == nil {
		return ErrGatewayAbsent
	}

	url := s.gatewayURL()
	s.log(LogLevelInfo, "Connecting using URL: %s", url)

	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return
	}
	s.conn = NewConnection(conn, compression.NewZstd())

	heartbeatCtx, cancelHeartbeat := context.WithCancel(ctx)
	defer cancelHeartbeat()

	err = s.expectPacket(ctx, types.GatewayOpHello, types.GatewayEventNone, s.handleHello(heartbeatCtx))
	if err != nil {
		return
	}

	seq, err := s.opts.Store.GetSeq(ctx, s.idUint())
	if err != nil {
		s.log(LogLevelWarn, "Unable to retrive sequence data for login: %s", err)
	}

	sessionID, err := s.opts.Store.GetSession(ctx, s.idUint())
	if err != nil {
		s.log(LogLevelWarn, "Unable to retrieve session ID for login: %s", err)
	}

	s.log(LogLevelDebug, "session \"%s\", seq %d", sessionID, seq)
	errs := make(chan error)

	go func() {
		if sessionID == "" && seq == 0 {
			if err = s.sendIdentify(); err != nil {
				errs <- err
			}
		} else {
			if err = s.sendResume(ctx); err != nil {
				errs <- err
			}
		}
	}()

	// mark shard as alive
	stats.ShardsAlive.WithLabelValues(s.id).Inc()
	defer stats.ShardsAlive.WithLabelValues(s.id).Dec()

	s.log(LogLevelDebug, "beginning normal message consumption")

	go func() {
		for {
			err = s.readPacket(ctx, nil)
			if err != nil {
				errs <- err
				break
			}
		}
	}()

	return <-errs
}

// CloseWithReason closes the connection and logs the reason
func (s *Shard) CloseWithReason(code int, reason error) error {
	s.log(LogLevelWarn, "%s: closing connection", reason)
	return s.conn.CloseWithCode(code)
}

// Close closes the current session
func (s *Shard) Close() (err error) {
	if err = s.conn.Close(); err != nil {
		return
	}

	s.log(LogLevelInfo, "Cleanly closed connection")
	return
}

func (s *Shard) readPacket(ctx context.Context, fn func(*types.ReceivePacket) error) (err error) {
	d, err := s.conn.Read()
	if err != nil {
		return
	}

	p := s.packets.Get().(*types.ReceivePacket)
	defer s.packets.Put(p)

	err = json.Unmarshal(d, p)
	if err != nil {
		return
	}

	// remove event from any previous OP 0s that used this packet
	if p.Op != types.GatewayOpDispatch {
		p.Event = ""
	}

	s.log(LogLevelDebug, "<- op:%d t:\"%s\"", p.Op, p.Event)

	// record packet received
	stats.PacketsReceived.WithLabelValues(string(p.Event), strconv.Itoa(int(p.Op)), s.id).Inc()

	if s.opts.OnPacket != nil {
		s.opts.OnPacket(p)
	}

	err = s.handlePacket(ctx, p)
	if err != nil {
		return
	}

	if fn != nil {
		err = fn(p)
	}
	return
}

// expectPacket reads the next packet, verifies its operation code, and event name (if applicable)
func (s *Shard) expectPacket(ctx context.Context, op types.GatewayOp, event types.GatewayEvent, handler func(*types.ReceivePacket) error) (err error) {
	err = s.readPacket(ctx, func(pk *types.ReceivePacket) error {
		if pk.Op != op {
			return fmt.Errorf("expected op to be %d, got %d", op, pk.Op)
		}

		if op == types.GatewayOpDispatch && pk.Event != event {
			return fmt.Errorf("expected event to be %s, got %s", event, pk.Event)
		}

		if handler != nil {
			return handler(pk)
		}

		return nil
	})

	return
}

// handlePacket handles a packet according to its operation code
func (s *Shard) handlePacket(ctx context.Context, p *types.ReceivePacket) (err error) {
	switch p.Op {
	case types.GatewayOpDispatch:
		return s.handleDispatch(ctx, p)

	case types.GatewayOpHeartbeat:
		return s.sendHeartbeat(ctx)

	case types.GatewayOpReconnect:
		if err = s.CloseWithReason(types.CloseUnknownError, ErrReconnectReceived); err != nil {
			return
		}

	case types.GatewayOpInvalidSession:
		resumable := new(bool)
		if err = json.Unmarshal(p.Data, resumable); err != nil {
			return
		}

		if *resumable {
			if err = s.sendResume(ctx); err != nil {
				return
			}

			s.log(LogLevelDebug, "Sent resume in response to invalid resumable session")
			return
		}

		time.Sleep(time.Second * time.Duration(rand.Intn(5)+1))
		if err = s.sendIdentify(); err != nil {
			return
		}

		s.log(LogLevelDebug, "Sent identify in response to invalid non-resumable session")

	case types.GatewayOpHeartbeatACK:
		if s.lastHeartbeat.Unix() != 0 {
			// record latest gateway ping
			s.Ping = time.Since(s.lastHeartbeat)
			stats.Ping.WithLabelValues(s.id).Observe(float64(s.Ping.Nanoseconds()) / 1e6)
		}

		s.log(LogLevelDebug, "Heartbeat ACK (RTT %s)", s.Ping)
		s.acks <- struct{}{}
	}

	return
}

// handleDispatch handles dispatch packets
func (s *Shard) handleDispatch(ctx context.Context, p *types.ReceivePacket) (err error) {
	if err = s.opts.Store.SetSeq(ctx, s.idUint(), uint(p.Seq)); err != nil {
		return
	}

	switch p.Event {
	case types.GatewayEventReady:
		r := new(types.Ready)
		if err = json.Unmarshal(p.Data, r); err != nil {
			return
		}

		s.resumeURL = r.ResumeGatewayURL
		
		if err = s.opts.Store.SetSession(ctx, s.idUint(), r.SessionID); err != nil {
			return
		}

		s.log(LogLevelDebug, "Session ID: %s", r.SessionID)
		s.log(LogLevelDebug, "Using version %d", r.Version)
		s.logTrace(r.Trace)

	case types.GatewayEventResumed:
		r := new(types.Resumed)
		if err = json.Unmarshal(p.Data, r); err != nil {
			return
		}

		s.logTrace(r.Trace)
	}

	return
}

func (s *Shard) handleHello(ctx context.Context) func(*types.ReceivePacket) error {
	return func(p *types.ReceivePacket) (err error) {
		h := new(types.Hello)
		if err = json.Unmarshal(p.Data, h); err != nil {
			return
		}

		s.logTrace(h.Trace)
		go s.startHeartbeater(ctx, time.Duration(h.HeartbeatInterval)*time.Millisecond)
		return
	}
}

// handleClose handles the WebSocket close event. Returns whether the session is recoverable.
func (s *Shard) handleClose(err error) (recoverable bool) {
	recoverable = !websocket.IsCloseError(
		err,
		types.CloseAuthenticationFailed,
		types.CloseInvalidShard,
		types.CloseShardingRequired,
		types.CloseInvalidAPIVersion,
		types.CloseInvalidIntents,
		types.CloseDisallowedIntents,
	)

	if recoverable {
		s.log(LogLevelInfo, "recoverable close: %s", err)
	} else {
		s.log(LogLevelInfo, "unrecoverable close: %s", err)
	}
	return
}

// SendPacket sends a packet
func (s *Shard) SendPacket(op types.GatewayOp, data interface{}) error {
	return s.Send(&types.SendPacket{
		Op:   op,
		Data: data,
	})
}

// Send sends a pre-prepared packet
func (s *Shard) Send(p *types.SendPacket) error {
	d, err := json.Marshal(p)
	if err != nil {
		return err
	}

	s.limiter.Lock()
	s.connMu.Lock()
	defer s.connMu.Unlock()

	// record packet sent
	defer stats.PacketsSent.WithLabelValues("", strconv.Itoa(int(p.Op)), s.id).Inc()

	s.log(LogLevelDebug, "-> op:%d d:%+v", p.Op, p.Data)
	_, err = s.conn.Write(d)
	return err
}

// sendIdentify sends an identify packet
func (s *Shard) sendIdentify() error {
	s.opts.IdentifyLimiter.Lock()
	return s.SendPacket(types.GatewayOpIdentify, s.opts.Identify)
}

// sendResume sends a resume packet
func (s *Shard) sendResume(ctx context.Context) error {
	sessionID, err := s.opts.Store.GetSession(ctx, s.idUint())
	if err != nil {
		return err
	}

	seq, err := s.opts.Store.GetSeq(ctx, s.idUint())
	if err != nil {
		return err
	}

	s.log(LogLevelDebug, "attempting to resume session")
	return s.SendPacket(types.GatewayOpResume, &types.Resume{
		Token:     s.opts.Identify.Token,
		SessionID: sessionID,
		Seq:       types.Seq(seq),
	})
}

// sendHeartbeat sends a heartbeat packet
func (s *Shard) sendHeartbeat(ctx context.Context) error {
	seq, err := s.opts.Store.GetSeq(ctx, s.idUint())
	if err != nil {
		return err
	}

	s.lastHeartbeat = time.Now()
	return s.SendPacket(types.GatewayOpHeartbeat, seq)
}

// startHeartbeater calls sendHeartbeat on the provided interval
func (s *Shard) startHeartbeater(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()

	acked := true
	s.log(LogLevelInfo, "starting heartbeat at interval %s", interval)
	defer s.log(LogLevelDebug, "stopping heartbeat timer")

	for {
		select {
		case <-s.acks:
			acked = true
		case <-t.C:
			if !acked {
				s.CloseWithReason(types.CloseSessionTimeout, ErrHeartbeatUnacknowledged)
				return
			}

			s.log(LogLevelDebug, "sending automatic heartbeat")
			if err := s.sendHeartbeat(ctx); err != nil {
				s.log(LogLevelError, "error sending automatic heartbeat: %s", err)
				return
			}
			acked = false

		case <-ctx.Done():
			return
		}
	}
}

// gatewayURL returns the Gateway URL with appropriate query parameters
func (s *Shard) gatewayURL() string {
	query := url.Values{
		"v":        {strconv.FormatUint(uint64(s.opts.Version), 10)},
		"encoding": {"json"},
		"compress": {"zstd-stream"},
	}

	if s.resumeURL != "" {
		return s.resumeURL + "/?" + query.Encode()
	} else {
		return s.Gateway.URL + "/?" + query.Encode()
	}
}

func (s *Shard) idUint() uint {
	return uint(s.opts.Identify.Shard[0])
}
