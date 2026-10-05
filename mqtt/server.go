package mqtt

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lsongdev/mqtt-go/proto"
)

// A Server holds all the state associated with an MQTT server.
type Server struct {
	// l             net.Listener
	subs          *subscriptions
	stats         *stats
	Done          chan struct{}
	StatsInterval time.Duration // Defaults to 10 seconds. Must be set using sync/atomic.StoreInt64().
	Dump          bool          // When true, dump the messages in and out.
	clientsMu     sync.Mutex
	clients       map[string]*incomingConn
	options       ServerOptions
	sessionsMu    sync.Mutex
	sessions      map[string]*sessionState
	clientSeq     uint64
	closeOnce     sync.Once
}

// ServerOptions enables broker features that require additional state.
// All features are off by default to preserve the small in-memory profile.
type ServerOptions struct {
	EnableQoS2                bool
	EnablePersistentSessions  bool
	EnableSharedSubscriptions bool
	SessionStore              SessionStore
	MaxPacketSize             int
}

func (s *Server) persistentSessionsEnabled() bool {
	return s.options.EnablePersistentSessions || s.options.SessionStore != nil
}

// NewServer creates a new MQTT server, which accepts connections from
// the given listener. When the server is stopped (for instance by
// another goroutine closing the net.Listener), channel Done will become
// readable.
func NewServer() *Server {
	svr, _ := NewServerWithOptions(ServerOptions{})
	return svr
}

// NewServerWithOptions creates a broker with explicitly enabled optional
// features. It restores unexpired sessions before accepting connections.
func NewServerWithOptions(options ServerOptions) (*Server, error) {
	svr := &Server{
		// l:             l,
		Done:          make(chan struct{}),
		subs:          newSubscriptions(),
		stats:         &stats{},
		clients:       make(map[string]*incomingConn),
		StatsInterval: time.Second * 10,
		options:       options,
		sessions:      make(map[string]*sessionState),
	}
	if options.SessionStore != nil {
		stored, err := options.SessionStore.List(context.Background())
		if err != nil {
			return nil, err
		}
		for i := range stored {
			state := &sessionState{StoredSession: stored[i], server: svr}
			for _, m := range state.Queue {
				if m.ID > state.nextQueueID {
					state.nextQueueID = m.ID
				}
			}
			svr.sessions[state.ClientID] = state
			for _, sub := range state.Subscriptions {
				svr.subs.addStored(sub, state, nil)
			}
		}
	}
	// start the stats reporting goroutine
	go svr.report()
	return svr, nil
}

func (svr *Server) report() {
	for {
		svr.stats.publish(svr.subs, svr.StatsInterval)
		select {
		case <-svr.Done:
			return
		default:
			// keep going
		}
		time.Sleep(svr.StatsInterval)
	}
}

// newIncomingConn creates a new incomingConn associated with this
// server. The connection becomes the property of the incomingConn
// and should not be touched again by the caller until the Done
// channel becomes readable.
func (s *Server) newIncomingConn(conn net.Conn) *incomingConn {
	return &incomingConn{
		svr:            s,
		conn:           conn,
		jobs:           make(chan job, sendingQueueLength),
		decode:         &proto.DecodeOptions{Version: proto.Version311, MaxPacketSize: s.options.MaxPacketSize},
		version:        proto.Version311,
		nextID:         1,
		incomingQoS2:   make(map[uint16]*proto.Publish),
		outgoingQoS2:   make(map[uint16]*proto.Publish),
		outgoingStored: make(map[uint16]uint64),
		packetIDs:      make(map[uint16]struct{}),
		closed:         make(chan struct{}),
		done:           make(chan struct{}),
	}
}

// Start makes the Server start accepting and handling connections.
func (s *Server) Start() {
	go func() {

	}()
}

func ListenAndServe(addr string, server *Server) (err error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return
	}
	return server.Serve(listener)
}

// Serve accepts MQTT connections from listener. Supplying the listener makes
// the broker easy to embed in applications and tests (including TLS or Unix
// sockets chosen by the caller).
func (s *Server) Serve(listener net.Listener) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		s.ServeConn(conn)
	}
}

// ServeConn hands an already-established transport to the broker.
func (s *Server) ServeConn(conn net.Conn) {
	cli := s.newIncomingConn(conn)
	s.stats.clientConnect()
	cli.start()
}

// Close stops reporting and closes active client transports. A caller-owned
// listener and SessionStore remain owned by the caller and must be closed
// separately.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		close(s.Done)
		s.clientsMu.Lock()
		connections := make([]net.Conn, 0, len(s.clients))
		for _, client := range s.clients {
			connections = append(connections, client.conn)
		}
		s.clientsMu.Unlock()
		for _, conn := range connections {
			_ = conn.Close()
		}
	})
	return nil
}

const sendingQueueLength = 10000

// An IncomingConn represents a connection into a Server.
type incomingConn struct {
	svr            *Server
	conn           net.Conn
	jobs           chan job
	clientid       string
	version        proto.ProtocolVersion
	decode         *proto.DecodeOptions
	nextID         uint16
	idMu           sync.Mutex
	packetIDs      map[uint16]struct{}
	session        *sessionState
	persistent     bool
	incomingQoS2   map[uint16]*proto.Publish
	outgoingQoS2   map[uint16]*proto.Publish
	outgoingStored map[uint16]uint64
	qosMu          sync.Mutex
	connected      bool
	keepAlive      time.Duration
	closed         chan struct{}
	closeOnce      sync.Once
	done           chan struct{}
}

// Start reading and writing on this connection.
func (c *incomingConn) start() {
	go c.reader()
	go c.writer()
}

// Add this connection to the map, or find out that an existing connection
// already exists for the same client-id.
func (c *incomingConn) add() *incomingConn {
	c.svr.clientsMu.Lock()
	defer c.svr.clientsMu.Unlock()

	existing, ok := c.svr.clients[c.clientid]
	if ok {
		// this client id already exists, return it
		return existing
	}

	c.svr.clients[c.clientid] = c
	return nil
}

// Delete a connection; the connection must be closed by the caller first.
func (c *incomingConn) del() {
	c.svr.clientsMu.Lock()
	if c.svr.clients[c.clientid] == c {
		delete(c.svr.clients, c.clientid)
	}
	c.svr.clientsMu.Unlock()
}

// Queue a message; no notification of sending is done.
func (c *incomingConn) stop() {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.conn.Close()
	})
}

func (c *incomingConn) submit(m proto.Message) bool {
	proto.SetVersion(m, c.version)
	j := job{m: m}
	select {
	case c.jobs <- j:
		return true
	case <-c.closed:
		return false
	default:
		// MQTT control packets cannot be dropped safely. A full per-client
		// queue therefore means the peer is too slow and the connection is
		// closed instead of silently corrupting protocol state.
		log.Print(c, ": outbound queue full; closing slow client")
		c.stop()
		return false
	}
}

// Queue a message, returns a channel that will be readable
// when the message is sent.
func (c *incomingConn) submitSync(m proto.Message) receipt {
	r := make(receipt, 1)
	j := job{m: m, r: r}
	select {
	case c.jobs <- j:
	case <-c.closed:
		r <- ErrClientClosed
		close(r)
	default:
		r <- ErrClientClosed
		close(r)
		c.stop()
	}
	return r
}

func (c *incomingConn) String() string {
	return fmt.Sprintf("{IncomingConn: %v}", c.clientid)
}

func (c *incomingConn) reader() {
	// On exit, close the connection and arrange for the writer to exit
	// by closing the output channel.
	defer func() {
		c.svr.stats.clientDisconnect()
		c.stop()
	}()

	for {
		if c.keepAlive > 0 {
			_ = c.conn.SetReadDeadline(time.Now().Add(c.keepAlive + c.keepAlive/2))
		}
		m, err := proto.DecodeOneMessage(c.conn, c.decode)
		if err != nil {
			if err == io.EOF {
				return
			}
			if strings.HasSuffix(err.Error(), "use of closed network connection") {
				return
			}
			log.Print("reader: ", err)
			return
		}
		c.svr.stats.messageRecv()

		if c.svr.Dump {
			log.Printf("dump  in: %T", m)
		}

		switch m := m.(type) {
		case *proto.Connect:
			if c.connected {
				return
			}
			c.version = proto.ProtocolVersion(m.ProtocolVersion)
			c.decode.Version = c.version
			rc := proto.RetCodeAccepted
			if !m.IsValidVersion() {
				log.Print("reader: reject connection from ", m.ProtocolName, " version ", m.ProtocolVersion)
				rc = proto.RetCodeUnacceptableProtocolVersion
			}

			// Check client id.
			if len(m.ClientId) < 1 && !m.CleanSession {
				rc = proto.RetCodeIdentifierRejected
			}
			assignedClientID := false
			if rc == proto.RetCodeAccepted && m.ClientId == "" {
				m.ClientId = fmt.Sprintf("mqtt-%d", atomic.AddUint64(&c.svr.clientSeq, 1))
				assignedClientID = true
			}
			c.clientid = m.ClientId
			c.keepAlive = time.Duration(m.KeepAliveTimer) * time.Second
			sessionPresent := false
			if rc == proto.RetCodeAccepted {
				// Only an accepted CONNECT may take over an existing ClientID.
				if existing := c.add(); existing != nil {
					if existing.version == proto.Version5 {
						r := existing.submitSync(&proto.Disconnect{ReasonCode: 0x8e})
						_ = r.wait()
					} else {
						existing.stop()
					}
					<-existing.done
					c.svr.clientsMu.Lock()
					c.svr.clients[c.clientid] = c
					c.svr.clientsMu.Unlock()
				}
				sessionPresent = c.svr.attachSession(c, m)
			}

			// TODO: Last will

			connack := &proto.ConnAck{ReturnCode: rc, SessionPresent: sessionPresent}
			if c.version == proto.Version5 && rc != proto.RetCodeAccepted {
				if rc == proto.RetCodeUnacceptableProtocolVersion {
					connack.ReturnCode = proto.ReturnCode(0x84)
				} else if rc == proto.RetCodeIdentifierRejected {
					connack.ReturnCode = proto.ReturnCode(0x85)
				}
			}
			if c.version == proto.Version5 && !c.svr.options.EnableQoS2 {
				connack.Properties = connack.Properties.Add(proto.PropertyMaximumQoS, byte(1))
			}
			if c.version == proto.Version5 {
				connack.Properties = connack.Properties.Add(proto.PropertySharedSubscriptionAvailable, boolToProperty(c.svr.options.EnableSharedSubscriptions))
				if assignedClientID {
					connack.Properties = connack.Properties.Add(proto.PropertyAssignedClientIdentifier, c.clientid)
				}
			}
			c.submit(connack)

			// close connection if it was a bad connect
			if rc != proto.RetCodeAccepted {
				log.Printf("Connection refused for %v: %v", c.conn.RemoteAddr(), ConnectionErrors[rc])
				return
			}
			c.connected = true
			c.deliverSessionQueue()

			// Log in mosquitto format.
			clean := 0
			if m.CleanSession {
				clean = 1
			}
			log.Printf("New client connected from %v as %v (c%v, k%v).", c.conn.RemoteAddr(), c.clientid, clean, m.KeepAliveTimer)

		case *proto.Publish:
			if !c.connected {
				return
			}
			if m.Header.QosLevel == proto.QosExactlyOnce {
				if !c.svr.options.EnableQoS2 || m.MessageId == 0 {
					return
				}
				c.qosMu.Lock()
				if _, exists := c.incomingQoS2[m.MessageId]; !exists {
					copy := *m
					c.incomingQoS2[m.MessageId] = &copy
				}
				c.qosMu.Unlock()
				c.submit(&proto.PubRec{MessageId: m.MessageId})
				continue
			}
			if m.Header.QosLevel != proto.QosAtMostOnce && m.MessageId == 0 {
				// Invalid message ID. See MQTT-2.3.1-1.
				log.Printf("reader: invalid MessageId in PUBLISH.")
				return
			}
			if isWildcard(m.TopicName) {
				log.Print("reader: ignoring PUBLISH with wildcard topic ", m.TopicName)
			} else {
				c.svr.subs.submit(c, m)
			}
			// https://github.com/jeffallen/mqtt/pull/16
			// https://docs.oasis-open.org/mqtt/mqtt/v3.1.1/os/mqtt-v3.1.1-os.html#_Toc398718041
			if m.Header.QosLevel == proto.QosAtLeastOnce {
				c.submit(&proto.PubAck{MessageId: m.MessageId})
			}

		case *proto.PingReq:
			if !c.connected {
				return
			}
			c.submit(&proto.PingResp{})

		case *proto.PubAck:
			// QoS 1 delivery is complete. Messages are held in memory only, so
			// there is no persistent inflight record to remove.
			c.ackOutgoing(m.MessageId)
			continue

		case *proto.PubRec:
			if !c.svr.options.EnableQoS2 {
				return
			}
			c.qosMu.Lock()
			_, ok := c.outgoingQoS2[m.MessageId]
			storedID := c.outgoingStored[m.MessageId]
			c.qosMu.Unlock()
			if ok {
				if storedID != 0 {
					c.svr.markSessionDelivery(c.session, storedID, m.MessageId, 1)
				}
				c.submit(&proto.PubRel{MessageId: m.MessageId})
			}

		case *proto.PubRel:
			if !c.svr.options.EnableQoS2 {
				return
			}
			c.qosMu.Lock()
			publish, ok := c.incomingQoS2[m.MessageId]
			if ok {
				delete(c.incomingQoS2, m.MessageId)
			}
			c.qosMu.Unlock()
			if ok {
				c.svr.subs.submit(c, publish)
			}
			c.submit(&proto.PubComp{MessageId: m.MessageId})

		case *proto.PubComp:
			if !c.svr.options.EnableQoS2 {
				return
			}
			c.qosMu.Lock()
			delete(c.outgoingQoS2, m.MessageId)
			c.qosMu.Unlock()
			c.ackOutgoing(m.MessageId)

		case *proto.Subscribe:
			if !c.connected {
				return
			}
			if m.Header.QosLevel != proto.QosAtLeastOnce {
				// protocol error, disconnect
				return
			}
			if m.MessageId == 0 {
				// Invalid message ID. See MQTT-2.3.1-1.
				log.Printf("reader: invalid MessageId in SUBSCRIBE.")
				return
			}
			suback := &proto.SubAck{
				MessageId: m.MessageId,
				TopicsQos: make([]proto.QosLevel, len(m.Topics)),
			}
			if c.version == proto.Version5 {
				suback.ReasonCodes = make([]proto.ReasonCode, len(m.Topics))
			}
			newSubscriptions := make([]bool, len(m.Topics))
			for i, tq := range m.Topics {
				group, filter, valid := parseSharedFilter(tq.Topic)
				sharedUnsupported := group != "" && !c.svr.options.EnableSharedSubscriptions
				if !valid || sharedUnsupported || (group != "" && tq.NoLocal) || filter == "" || (isWildcard(filter) && !newWild(filter, nil).valid()) {
					suback.TopicsQos[i] = proto.QosLevel(0x80)
					if c.version == proto.Version5 {
						if sharedUnsupported {
							suback.ReasonCodes[i] = 0x9e
						} else {
							suback.ReasonCodes[i] = 0x8f
						}
					}
					continue
				}
				granted := tq.Qos
				if granted > proto.QosAtLeastOnce && !c.svr.options.EnableQoS2 {
					granted = proto.QosAtLeastOnce
				}
				tq.Qos = granted
				newSubscriptions[i] = c.svr.subs.add(tq, c)
				c.svr.recordSubscription(c, tq)
				suback.TopicsQos[i] = granted
				if c.version == proto.Version5 {
					suback.ReasonCodes[i] = proto.ReasonCode(granted)
				}
			}
			c.submit(suback)

			// Process retained messages.
			for i, tq := range m.Topics {
				if suback.TopicsQos[i] != proto.QosLevel(0x80) && tq.RetainHandling != 2 && (tq.RetainHandling == 0 || newSubscriptions[i]) {
					c.svr.subs.sendRetain(tq, c)
				}
			}

		case *proto.Unsubscribe:
			if !c.connected {
				return
			}
			if m.Header.QosLevel != proto.QosAtMostOnce && m.MessageId == 0 {
				// Invalid message ID. See MQTT-2.3.1-1.
				log.Printf("reader: invalid MessageId in UNSUBSCRIBE.")
				return
			}
			for _, t := range m.Topics {
				c.svr.subs.unsub(t, c)
				c.svr.removeSubscription(c, t)
			}
			ack := &proto.UnsubAck{MessageId: m.MessageId}
			if c.version == proto.Version5 {
				ack.ReasonCodes = make([]proto.ReasonCode, len(m.Topics))
			}
			c.submit(ack)

		case *proto.Disconnect:
			c.svr.updateSessionExpiry(c, m.Properties)
			return

		default:
			log.Printf("reader: unknown msg type %T", m)
			return
		}
	}
}

func (c *incomingConn) writer() {
	defer func() {
		c.stop()
		c.del()
		c.svr.detachSession(c)
		close(c.done)
	}()

	for {
		var job job
		select {
		case job = <-c.jobs:
		case <-c.closed:
			return
		}
		if c.svr.Dump {
			log.Printf("dump out: %T", job.m)
		}

		// TODO: write timeout
		err := job.m.Encode(c.conn)
		if job.r != nil {
			job.r <- err
			close(job.r)
		}
		if err != nil {
			// This one is not interesting; it happens when clients
			// disappear before we send their acks.
			oe, isoe := err.(*net.OpError)
			if isoe && oe.Err.Error() == "use of closed network connection" {
				return
			}
			// In Go < 1.5, the error is not an OpError.
			if err.Error() == "use of closed network connection" {
				return
			}

			log.Print("writer: ", err)
			return
		}
		c.svr.stats.messageSend()

		if _, ok := job.m.(*proto.Disconnect); ok {
			log.Print("writer: sent disconnect message")
			return
		}
	}
}

// A retain holds information necessary to correctly manage retained
// messages.
//
// This needs to hold copies of the proto.Publish, not pointers to
// it, or else we can send out one with the wrong retain flag.
type retain struct {
	m proto.Publish
}

// A post is a unit of work for the subscription processing workers.
type post struct {
	c *incomingConn
	m *proto.Publish
}

type subscriptions struct {
	mu         sync.Mutex // guards access to fields below
	posts      chan post
	retain     map[string]retain
	subs       map[string][]subscription // topic <-> conns
	wildcards  []wild
	roundRobin map[string]uint64
}

type subscription struct {
	c                          *incomingConn
	qos                        proto.QosLevel
	noLocal, retainAsPublished bool
	session                    *sessionState
	filter, shareGroup         string
}

func (s subscription) clientID() string {
	if s.session != nil {
		return s.session.ClientID
	}
	if s.c != nil {
		return s.c.clientid
	}
	return ""
}

// The length of the ordered subscription dispatcher queue.
const postQueue = 100

func newSubscriptions() *subscriptions {
	s := &subscriptions{
		subs:       make(map[string][]subscription),
		retain:     make(map[string]retain),
		posts:      make(chan post, postQueue),
		roundRobin: make(map[string]uint64),
	}
	// One dispatcher preserves ordered-topic delivery. Socket writes remain
	// parallel because each connection has its own writer goroutine.
	go s.run()
	return s
}

func (s *subscriptions) sendRetain(tq proto.TopicQos, c *incomingConn) {
	s.mu.Lock()
	var messages []proto.Publish
	_, filter, _ := parseSharedFilter(tq.Topic)
	w := newWild(filter, nil)
	for name, retained := range s.retain {
		if strings.HasPrefix(name, "$") && !strings.HasPrefix(filter, "$") {
			continue
		}
		if (!isWildcard(filter) && name == filter) || (isWildcard(filter) && w.matches(strings.Split(name, "/"))) {
			m := retained.m
			if m.QosLevel > tq.Qos {
				m.QosLevel = tq.Qos
			}
			if m.QosLevel.HasId() {
				m.MessageId = c.nextMessageID()
				if m.MessageId == 0 {
					continue
				}
			}
			messages = append(messages, m)
		}
	}
	s.mu.Unlock()
	for i := range messages {
		storedID := uint64(0)
		if c.persistent && messages[i].QosLevel > proto.QosAtMostOnce {
			if stored, ok := storedPublish(&messages[i]); ok {
				storedID = c.svr.queueSession(c.session, stored)
			}
		}
		c.trackAndSubmit(&messages[i], storedID)
	}
}

func (s *subscriptions) add(tq proto.TopicQos, c *incomingConn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	group, filter, _ := parseSharedFilter(tq.Topic)
	sub := subscription{c: c, qos: tq.Qos, noLocal: tq.NoLocal, retainAsPublished: tq.RetainAsPublished, session: c.session, filter: tq.Topic, shareGroup: group}
	if isWildcard(filter) {
		for i := range s.wildcards {
			if s.wildcards[i].sub.clientID() == c.clientid && s.wildcards[i].sub.filter == tq.Topic {
				s.wildcards[i].sub = sub
				return false
			}
		}
		w := newWild(filter, c)
		w.sub = sub
		if w.valid() {
			s.wildcards = append(s.wildcards, w)
		} else {
			log.Println("invalid wildcard", tq.Topic)
		}
		return true
	}
	for i := range s.subs[filter] {
		if s.subs[filter][i].clientID() == c.clientid && s.subs[filter][i].filter == tq.Topic {
			s.subs[filter][i] = sub
			return false
		}
	}
	s.subs[filter] = append(s.subs[filter], sub)
	return true
}

func (s *subscriptions) addStored(stored StoredSubscription, state *sessionState, c *incomingConn) {
	tq := proto.TopicQos{Topic: stored.Filter, Qos: stored.QoS, NoLocal: stored.NoLocal, RetainAsPublished: stored.RetainAsPublished, RetainHandling: stored.RetainHandling}
	placeholder := &incomingConn{clientid: state.ClientID, session: state}
	if c != nil {
		placeholder = c
	}
	s.add(tq, placeholder)
	if c == nil {
		s.detach(placeholder, true)
	}
}

func (s *subscriptions) bind(clientID string, c *incomingConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for topic, list := range s.subs {
		for i := range list {
			if list[i].clientID() == clientID {
				list[i].c = c
				list[i].session = c.session
			}
		}
		s.subs[topic] = list
	}
	for i := range s.wildcards {
		if s.wildcards[i].sub.clientID() == clientID {
			s.wildcards[i].sub.c = c
			s.wildcards[i].sub.session = c.session
		}
	}
}

func (s *subscriptions) removeClient(clientID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for topic, list := range s.subs {
		out := list[:0]
		for _, sub := range list {
			if sub.clientID() != clientID {
				out = append(out, sub)
			}
		}
		if len(out) == 0 {
			delete(s.subs, topic)
		} else {
			s.subs[topic] = out
		}
	}
	out := s.wildcards[:0]
	for _, w := range s.wildcards {
		if w.sub.clientID() != clientID {
			out = append(out, w)
		}
	}
	s.wildcards = out
}

func (s *subscriptions) detach(c *incomingConn, preserve bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for topic, list := range s.subs {
		out := list[:0]
		for _, sub := range list {
			if sub.c == c {
				if preserve {
					sub.c = nil
					out = append(out, sub)
				}
			} else {
				out = append(out, sub)
			}
		}
		if len(out) == 0 {
			delete(s.subs, topic)
		} else {
			s.subs[topic] = out
		}
	}
	out := s.wildcards[:0]
	for _, w := range s.wildcards {
		if w.sub.c == c {
			if preserve {
				w.sub.c = nil
				out = append(out, w)
			}
		} else {
			out = append(out, w)
		}
	}
	s.wildcards = out
}

// Find all connections that are subscribed to this topic.
func (s *subscriptions) subscribers(topic string) []subscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	// non-wildcard subscribers
	candidates := append([]subscription(nil), s.subs[topic]...)
	// process wildcards
	parts := strings.Split(topic, "/")
	for _, w := range s.wildcards {
		// A leading wildcard does not match Topic Names beginning with '$'.
		// Shared subscriptions store the inner filter in w.wild.
		if strings.HasPrefix(topic, "$") && (len(w.wild) == 0 || !strings.HasPrefix(w.wild[0], "$")) {
			continue
		}
		if w.matches(parts) {
			candidates = append(candidates, w.sub)
		}
	}
	var subscribers []subscription
	groups := make(map[string][]subscription)
	for _, sub := range candidates {
		if sub.shareGroup == "" {
			subscribers = append(subscribers, sub)
		} else {
			key := sub.shareGroup + "\x00" + sub.filter
			groups[key] = append(groups[key], sub)
		}
	}
	for key, list := range groups {
		var online []subscription
		for _, sub := range list {
			if sub.c != nil {
				online = append(online, sub)
			}
		}
		if len(online) > 0 {
			list = online
		}
		index := s.roundRobin[key] % uint64(len(list))
		s.roundRobin[key]++
		subscribers = append(subscribers, list[index])
	}
	return subscribers
}

// Remove all subscriptions that refer to a connection.
func (s *subscriptions) unsubAll(c *incomingConn) {
	s.detach(c, false)
}

// Remove the subscription to topic for a given connection.
func (s *subscriptions) unsub(topic string, c *incomingConn) {
	s.mu.Lock()
	_, filter, _ := parseSharedFilter(topic)
	if isWildcard(filter) {
		filtered := s.wildcards[:0]
		for _, w := range s.wildcards {
			if w.sub.clientID() != c.clientid || w.sub.filter != topic {
				filtered = append(filtered, w)
			}
		}
		s.wildcards = filtered
		s.mu.Unlock()
		return
	}
	if conns, ok := s.subs[filter]; ok {
		out := conns[:0]
		for _, sub := range conns {
			if sub.clientID() != c.clientid || sub.filter != topic {
				out = append(out, sub)
			}
		}
		if len(out) == 0 {
			delete(s.subs, filter)
		} else {
			s.subs[filter] = out
		}
	}
	s.mu.Unlock()
}

// run is the ordered subscription dispatcher.
func (s *subscriptions) run() {
	for post := range s.posts {
		// Remember the original retain setting, but send out immediate
		// copies without retain: "When a server sends a PUBLISH to a client
		// as a result of a subscription that already existed when the
		// original PUBLISH arrived, the Retain flag should not be set,
		// regardless of the Retain flag of the original PUBLISH.
		isRetain := post.m.Header.Retain

		// Handle "retain with payload size zero = delete retain".
		// Once the delete is done, return instead of continuing.
		deleteRetained := isRetain && post.m.Payload.Size() == 0
		if deleteRetained {
			s.mu.Lock()
			delete(s.retain, post.m.TopicName)
			s.mu.Unlock()
		}
		// Find all the connections that should be notified of this message.
		conns := s.subscribers(post.m.TopicName)
		// Queue the outgoing messages
		for _, sub := range conns {
			if sub.c == post.c && sub.noLocal {
				continue
			}

			out := *post.m
			if out.QosLevel > sub.qos {
				out.QosLevel = sub.qos
			}
			if !sub.retainAsPublished {
				out.Retain = false
			}
			if sub.c != nil {
				if out.QosLevel.HasId() {
					out.MessageId = sub.c.nextMessageID()
					if out.MessageId == 0 {
						continue
					}
				} else {
					out.MessageId = 0
				}
				storedID := uint64(0)
				if sub.session != nil && sub.c.persistent && out.QosLevel > proto.QosAtMostOnce {
					if stored, ok := storedPublish(&out); ok {
						storedID = sub.session.server.queueSession(sub.session, stored)
					}
				}
				sub.c.trackAndSubmit(&out, storedID)
			} else if sub.session != nil && out.QosLevel > proto.QosAtMostOnce && sub.session.server.sessionActive(sub.session) {
				if stored, ok := storedPublish(&out); ok {
					_ = sub.session.server.queueSession(sub.session, stored)
				}
			}
		}

		if isRetain && !deleteRetained {
			s.mu.Lock()
			// Save a copy of it, and set that copy's Retain to true, so that
			// when we send it out later we notify new subscribers that this
			// is an old message.
			msg := *post.m
			msg.Header.Retain = true
			s.retain[post.m.TopicName] = retain{m: msg}
			s.mu.Unlock()
		}
	}
}

func (s *subscriptions) submit(c *incomingConn, m *proto.Publish) {
	s.posts <- post{c: c, m: m}
}

func isWildcard(topic string) bool {
	if strings.Contains(topic, "#") || strings.Contains(topic, "+") {
		return true
	}
	return false
}

func parseSharedFilter(filter string) (group, inner string, valid bool) {
	if !strings.HasPrefix(filter, "$share/") {
		return "", filter, true
	}
	rest := strings.TrimPrefix(filter, "$share/")
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 || slash == len(rest)-1 {
		return "", "", false
	}
	group, inner = rest[:slash], rest[slash+1:]
	if strings.ContainsAny(group, "+#") || strings.HasPrefix(inner, "$share/") {
		return "", "", false
	}
	return group, inner, true
}

type wild struct {
	wild []string
	sub  subscription
}

func newWild(topic string, c *incomingConn) wild {
	return wild{wild: strings.Split(topic, "/"), sub: subscription{c: c}}
}

func (w wild) matches(parts []string) bool {
	i := 0
	for i < len(parts) {
		// topic is longer, no match
		if i >= len(w.wild) {
			return false
		}
		// matched up to here, and now the wildcard says "all others will match"
		if w.wild[i] == "#" {
			return true
		}
		// text does not match, and there wasn't a + to excuse it
		if parts[i] != w.wild[i] && w.wild[i] != "+" {
			return false
		}
		i++
	}

	// make finance/stock/ibm/# match finance/stock/ibm
	if i == len(w.wild)-1 && w.wild[len(w.wild)-1] == "#" {
		return true
	}

	if i == len(w.wild) {
		return true
	}
	return false
}

func (w wild) valid() bool {
	for i, part := range w.wild {
		// catch things like finance#
		if isWildcard(part) && len(part) != 1 {
			return false
		}
		// # can only occur as the last part
		if part == "#" && i != len(w.wild)-1 {
			return false
		}
	}
	return true
}

// An intPayload implements proto.Payload, and is an int64 that
// formats itself and then prints itself into the payload.
type intPayload string

func newIntPayload(i int64) intPayload {
	return intPayload(fmt.Sprint(i))
}
func (ip intPayload) ReadPayload(r io.Reader) error {
	// not implemented
	return nil
}
func (ip intPayload) WritePayload(w io.Writer) error {
	_, err := w.Write([]byte(string(ip)))
	return err
}
func (ip intPayload) Size() int {
	return len(ip)
}

func (c *incomingConn) nextMessageID() uint16 {
	c.idMu.Lock()
	defer c.idMu.Unlock()
	for i := 0; i < 65535; i++ {
		if c.nextID == 0 {
			c.nextID = 1
		}
		id := c.nextID
		c.nextID++
		if _, used := c.packetIDs[id]; used {
			continue
		}
		c.packetIDs[id] = struct{}{}
		return id
	}
	c.stop()
	return 0
}

func (c *incomingConn) reserveMessageID(id uint16) bool {
	if id == 0 {
		return false
	}
	c.idMu.Lock()
	defer c.idMu.Unlock()
	if _, used := c.packetIDs[id]; used {
		return true
	}
	c.packetIDs[id] = struct{}{}
	if id >= c.nextID {
		c.nextID = id + 1
		if c.nextID == 0 {
			c.nextID = 1
		}
	}
	return true
}

func (c *incomingConn) releaseMessageID(id uint16) {
	if id == 0 {
		return
	}
	c.idMu.Lock()
	delete(c.packetIDs, id)
	c.idMu.Unlock()
}

type stats struct {
	recv       int64
	sent       int64
	clients    int64
	clientsMax int64
	lastmsgs   int64
}

func (s *stats) messageRecv()      { atomic.AddInt64(&s.recv, 1) }
func (s *stats) messageSend()      { atomic.AddInt64(&s.sent, 1) }
func (s *stats) clientConnect()    { atomic.AddInt64(&s.clients, 1) }
func (s *stats) clientDisconnect() { atomic.AddInt64(&s.clients, -1) }

func statsMessage(topic string, stat int64) *proto.Publish {
	return &proto.Publish{
		Header:    header(dupFalse, proto.QosAtMostOnce, retainTrue),
		TopicName: topic,
		Payload:   newIntPayload(stat),
	}
}

func (s *stats) publish(sub *subscriptions, interval time.Duration) {
	clients := atomic.LoadInt64(&s.clients)
	clientsMax := atomic.LoadInt64(&s.clientsMax)
	if clients > clientsMax {
		clientsMax = clients
		atomic.StoreInt64(&s.clientsMax, clientsMax)
	}
	sub.submit(nil, statsMessage("$SYS/broker/clients/active", clients))
	sub.submit(nil, statsMessage("$SYS/broker/clients/maximum", clientsMax))
	sub.submit(nil, statsMessage("$SYS/broker/messages/received",
		atomic.LoadInt64(&s.recv)))
	sub.submit(nil, statsMessage("$SYS/broker/messages/sent",
		atomic.LoadInt64(&s.sent)))

	msgs := atomic.LoadInt64(&s.recv) + atomic.LoadInt64(&s.sent)
	msgpersec := (msgs - s.lastmsgs) / int64(interval/time.Second)
	// no need for atomic because we are the only reader/writer of it
	s.lastmsgs = msgs

	sub.submit(nil, statsMessage("$SYS/broker/messages/per-sec", msgpersec))
}
