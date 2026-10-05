package mqtt

import (
	"context"
	crand "crypto/rand"

	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/lsongdev/mqtt-go/proto"
)

// A random number generator ready to make client-id's, if
// they do not provide them to us.
var cliRand *rand.Rand

func init() {
	var seed int64
	var sb [4]byte
	crand.Read(sb[:])
	seed = int64(time.Now().Nanosecond())<<32 |
		int64(sb[0])<<24 | int64(sb[1])<<16 |
		int64(sb[2])<<8 | int64(sb[3])
	cliRand = rand.New(rand.NewSource(seed))
}

// A ClientConn holds all the state associated with a connection
// to an MQTT server. It should be allocated via NewClientConn.
// Concurrent access to a ClientConn is NOT safe.
type ClientConn struct {
	conn            net.Conn
	ClientId        string // May be set before the call to Connect.
	id              uint16 // next packet identifier
	idMu            sync.Mutex
	packetIDs       map[uint16]struct{}
	done            chan struct{} // This channel will be readable once a Disconnect has been successfully sent and the connection is closed.
	closed          chan struct{}
	out             chan job
	Incoming        chan *proto.Publish // Incoming messages arrive on this channel.
	connack         chan *proto.ConnAck
	suback          chan *proto.SubAck
	unsuback        chan *proto.UnsubAck
	pingresp        chan struct{}
	Dump            bool                  // When true, dump the messages in and out.
	ProtocolVersion proto.ProtocolVersion // Defaults to MQTT 3.1.1 (level 4).
	decode          *proto.DecodeOptions
	EnableQoS2      bool
	SessionPresent  bool
	keepAlive       time.Duration
	keepAliveOnce   sync.Once
	qosMu           sync.Mutex
	incomingQoS2    map[uint16]*proto.Publish
	outgoingQoS2    map[uint16]*proto.Publish
	maximumQoS      proto.QosLevel
}

// ClientOptions configures the modern Dial API.
type ClientOptions struct {
	ProtocolVersion proto.ProtocolVersion
	ClientID        string
	Username        string
	Password        string
	CleanStart      bool
	KeepAlive       uint16
	Properties      proto.Properties
	Will            *Will
	EnableQoS2      bool
	SessionExpiry   time.Duration
	MaxPacketSize   int
}

func propertyString(properties proto.Properties, id proto.PropertyID) (string, bool) {
	for _, p := range properties {
		if p.ID == id {
			value, ok := p.Value.(string)
			return value, ok
		}
	}
	return "", false
}

func (o ClientOptions) normalized() ClientOptions {
	if o.ProtocolVersion == 0 {
		o.ProtocolVersion = proto.Version311
	}
	if o.MaxPacketSize == 0 {
		o.MaxPacketSize = DefaultMaxPacketSize
	}
	if o.ClientID == "" && o.ProtocolVersion != proto.Version5 {
		o.CleanStart = true
	}
	return o
}

const clientQueueLength = 100

// NewClientConn allocates a new ClientConn.
func NewClientConn(c net.Conn) *ClientConn {
	decode := &proto.DecodeOptions{Version: proto.Version311}
	cc := &ClientConn{
		conn:            c,
		id:              1,
		packetIDs:       make(map[uint16]struct{}),
		out:             make(chan job, clientQueueLength),
		Incoming:        make(chan *proto.Publish, clientQueueLength),
		done:            make(chan struct{}),
		closed:          make(chan struct{}),
		connack:         make(chan *proto.ConnAck),
		suback:          make(chan *proto.SubAck),
		unsuback:        make(chan *proto.UnsubAck),
		pingresp:        make(chan struct{}, 1),
		ProtocolVersion: proto.Version311,
		decode:          decode,
		incomingQoS2:    make(map[uint16]*proto.Publish),
		outgoingQoS2:    make(map[uint16]*proto.Publish),
		maximumQoS:      proto.QosExactlyOnce,
	}
	go cc.reader()
	go cc.writer()
	return cc
}

// Dial connects and completes the MQTT handshake in one call. It is the
// recommended API for applications embedding the client.
func Dial(ctx context.Context, address string, options ClientOptions) (*ClientConn, error) {
	options = options.normalized()
	nc, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	c := NewClientConn(nc)
	if err := c.ConnectContext(ctx, options); err != nil {
		nc.Close()
		return nil, err
	}
	return c, nil
}

func NewClient(host string) (conn *ClientConn, err error) {
	c, err := net.Dial("tcp", host)
	if err != nil {
		return nil, err
	}
	conn = NewClientConn(c)
	return
}

func (c *ClientConn) reader() {
	defer func() {
		// Cause any goroutines waiting on messages to arrive to exit.
		close(c.Incoming)
		close(c.closed)
		c.conn.Close()
	}()

	for {
		m, err := proto.DecodeOneMessage(c.conn, c.decode)
		if err != nil {
			if err == io.EOF {
				return
			}
			if strings.HasSuffix(err.Error(), "use of closed network connection") {
				return
			}
			log.Print("cli reader: ", err)
			return
		}

		if c.Dump {
			log.Printf("dump  in: %T", m)
		}

		switch m := m.(type) {
		case *proto.Publish:
			if m.QosLevel == proto.QosExactlyOnce {
				if !c.EnableQoS2 {
					return
				}
				c.qosMu.Lock()
				if _, exists := c.incomingQoS2[m.MessageId]; !exists {
					copy := *m
					c.incomingQoS2[m.MessageId] = &copy
				}
				c.qosMu.Unlock()
				c.send(&proto.PubRec{MessageId: m.MessageId})
				continue
			}
			if m.QosLevel == proto.QosAtLeastOnce {
				c.send(&proto.PubAck{MessageId: m.MessageId})
			}
			c.Incoming <- m
		case *proto.PubAck:
			c.releaseid(m.MessageId)
			continue
		case *proto.PubRec:
			c.qosMu.Lock()
			_, ok := c.outgoingQoS2[m.MessageId]
			if m.ReasonCode >= 0x80 {
				delete(c.outgoingQoS2, m.MessageId)
			}
			c.qosMu.Unlock()
			if m.ReasonCode >= 0x80 {
				c.releaseid(m.MessageId)
				continue
			}
			if ok {
				c.send(&proto.PubRel{MessageId: m.MessageId})
			}
		case *proto.PubRel:
			c.qosMu.Lock()
			publish, ok := c.incomingQoS2[m.MessageId]
			if ok {
				delete(c.incomingQoS2, m.MessageId)
			}
			c.qosMu.Unlock()
			if ok {
				c.Incoming <- publish
			}
			c.send(&proto.PubComp{MessageId: m.MessageId})
		case *proto.PubComp:
			c.qosMu.Lock()
			delete(c.outgoingQoS2, m.MessageId)
			c.qosMu.Unlock()
			c.releaseid(m.MessageId)
		case *proto.ConnAck:
			c.connack <- m
		case *proto.SubAck:
			c.releaseid(m.MessageId)
			c.suback <- m
		case *proto.UnsubAck:
			c.releaseid(m.MessageId)
			c.unsuback <- m
		case *proto.PingResp:
			select {
			case c.pingresp <- struct{}{}:
			default:
			}
			continue
		case *proto.Disconnect:
			return
		default:
			log.Printf("cli reader: got msg type %T", m)
		}
	}
}

func (c *ClientConn) writer() {
	// Close connection on exit in order to cause reader to exit.
	defer func() {
		// Signal to Disconnect() that the message is on its way, or
		// that the connection is closing one way or the other...
		close(c.done)
		c.conn.Close()
	}()

	for {
		var next job
		select {
		case next = <-c.out:
		case <-c.closed:
			return
		}
		job := next
		if c.Dump {
			log.Printf("dump out: %T", job.m)
		}

		proto.SetVersion(job.m, c.ProtocolVersion)
		err := job.m.Encode(c.conn)
		if job.r != nil {
			job.r <- err
			close(job.r)
		}

		if err != nil {
			log.Print("cli writer: ", err)
			return
		}

		if _, ok := job.m.(*proto.Disconnect); ok {
			return
		}
	}
}

// Connect sends the CONNECT message to the server. If the ClientId is not already
// set, use a default (a 63-bit decimal random number). The "clean session"
// bit is always set.
func (c *ClientConn) Connect(user, pass string) error {
	return c.ConnectWithOptions(ClientOptions{ProtocolVersion: c.ProtocolVersion, ClientID: c.ClientId, Username: user, Password: pass, CleanStart: true})
}

// ConnectWithOptions sends a versioned CONNECT packet on an existing transport.
func (c *ClientConn) ConnectWithOptions(options ClientOptions) error {
	options = options.normalized()
	requestedAssignedID := options.ClientID == "" && options.ProtocolVersion == proto.Version5
	if options.ClientID == "" && options.ProtocolVersion != proto.Version5 {
		options.ClientID = fmt.Sprint(cliRand.Int63())
	}
	c.ClientId = options.ClientID
	c.ProtocolVersion = options.ProtocolVersion
	c.EnableQoS2 = options.EnableQoS2
	c.decode.Version = options.ProtocolVersion
	c.decode.MaxPacketSize = options.MaxPacketSize
	req := &proto.Connect{
		ProtocolName:    proto.PROTOCOL_3_1_1,
		ProtocolVersion: uint8(options.ProtocolVersion),
		ClientId:        c.ClientId,
		CleanSession:    options.CleanStart,
		KeepAliveTimer:  options.KeepAlive,
		Properties:      options.Properties,
	}
	if options.Will != nil {
		req.WillFlag = true
		req.WillQos = options.Will.QoS
		req.WillRetain = options.Will.Retain
		req.WillTopic = options.Will.Topic
		req.WillMessage = string(options.Will.Payload)
		if options.ProtocolVersion == proto.Version5 {
			req.WillProperties = append(proto.Properties(nil), options.Will.Properties...)
		}
	}
	if options.Username != "" {
		req.UsernameFlag = true
		req.Username = options.Username
	}
	if options.Password != "" {
		req.PasswordFlag = true
		req.Password = options.Password
	}
	if options.ProtocolVersion == proto.Version5 && options.SessionExpiry > 0 {
		if _, exists := propertyUint32(req.Properties, proto.PropertySessionExpiryInterval); !exists {
			seconds := uint64((options.SessionExpiry + time.Second - 1) / time.Second)
			if seconds > uint64(^uint32(0)) {
				seconds = uint64(^uint32(0))
			}
			req.Properties = req.Properties.Add(proto.PropertySessionExpiryInterval, uint32(seconds))
		}
	}
	if err := req.Validate(); err != nil {
		return err
	}

	if err := c.sync(req); err != nil {
		return err
	}
	var ack *proto.ConnAck
	select {
	case ack = <-c.connack:
	case <-c.closed:
		return ErrClientClosed
	}
	if ack.ReturnCode == proto.RetCodeAccepted {
		c.SessionPresent = ack.SessionPresent
		if options.ProtocolVersion == proto.Version5 {
			for _, property := range ack.Properties {
				if property.ID == proto.PropertyMaximumQoS {
					c.maximumQoS = proto.QosLevel(property.Value.(byte))
				}
			}
		}
		if requestedAssignedID {
			assigned, ok := propertyString(ack.Properties, proto.PropertyAssignedClientIdentifier)
			if !ok || assigned == "" {
				_ = c.conn.Close()
				return fmt.Errorf("mqtt: server accepted empty client id without Assigned Client Identifier")
			}
			c.ClientId = assigned
		}
		keepAlive := options.KeepAlive
		if options.ProtocolVersion == proto.Version5 {
			if value, ok := propertyUint16(ack.Properties, proto.PropertyServerKeepAlive); ok {
				keepAlive = value
			}
		}
		if keepAlive > 0 {
			c.keepAlive = time.Duration(keepAlive) * time.Second
			c.keepAliveOnce.Do(func() { go c.keepAliveLoop() })
		}
		return nil
	}
	if int(ack.ReturnCode) < len(ConnectionErrors) {
		return ConnectionErrors[ack.ReturnCode]
	}
	switch ack.ReturnCode {
	case proto.ReturnCode(0x86):
		return ErrBadCredentials
	case proto.ReturnCode(0x87):
		return ErrNotAuthorized
	}
	return fmt.Errorf("connection refused: reason code 0x%02x", uint8(ack.ReturnCode))
}

// ConnectContext is ConnectWithOptions with cancellation and deadline support.
func (c *ClientConn) ConnectContext(ctx context.Context, options ClientOptions) error {
	stop := context.AfterFunc(ctx, func() { _ = c.conn.SetDeadline(time.Now()) })
	defer func() { stop(); _ = c.conn.SetDeadline(time.Time{}) }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.conn.SetDeadline(deadline)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	err := c.ConnectWithOptions(options)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if deadline, ok := ctx.Deadline(); err != nil && ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return err
}

// Disconnect sends a DISCONNECT message to the server. This function
// blocks until the disconnect message is actually sent, and the connection
// is closed.
func (c *ClientConn) Disconnect() {
	select {
	case <-c.closed:
		<-c.done
		return
	default:
	}
	m := &proto.Disconnect{}
	proto.SetVersion(m, c.ProtocolVersion)
	_ = c.sync(m)
	<-c.done
}

// Close immediately closes the underlying transport.
func (c *ClientConn) Close() error { return c.conn.Close() }

func (c *ClientConn) nextid() (uint16, error) {
	c.idMu.Lock()
	defer c.idMu.Unlock()
	for i := 0; i < 65535; i++ {
		if c.id == 0 {
			c.id = 1
		}
		id := c.id
		c.id++
		if _, used := c.packetIDs[id]; used {
			continue
		}
		c.packetIDs[id] = struct{}{}
		return id, nil
	}
	return 0, ErrPacketIdentifiersExhausted
}

func (c *ClientConn) releaseid(id uint16) {
	if id == 0 {
		return
	}
	c.idMu.Lock()
	delete(c.packetIDs, id)
	c.idMu.Unlock()
}

func (c *ClientConn) send(m proto.Message) bool {
	select {
	case c.out <- job{m: m}:
		return true
	case <-c.closed:
		return false
	}
}

func (c *ClientConn) keepAliveLoop() {
	ticker := time.NewTicker(c.keepAlive)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			// Discard a stale response before starting a new ping exchange.
			select {
			case <-c.pingresp:
			default:
			}
			if err := c.sync(&proto.PingReq{}); err != nil {
				return
			}
			timer := time.NewTimer(c.keepAlive)
			select {
			case <-c.pingresp:
				if !timer.Stop() {
					<-timer.C
				}
			case <-timer.C:
				_ = c.conn.Close()
				return
			case <-c.closed:
				if !timer.Stop() {
					<-timer.C
				}
				return
			}
		case <-c.closed:
			return
		}
	}
}

// Subscribe subscribes this connection to a list of topics. Messages
// will be delivered on the Incoming channel.
func (c *ClientConn) Subscribe(tqs []proto.TopicQos) *proto.SubAck {
	id, err := c.nextid()
	if err != nil {
		return nil
	}
	m := &proto.Subscribe{
		Header:    header(dupFalse, proto.QosAtLeastOnce, retainFalse),
		MessageId: id,
		Topics:    tqs,
	}
	proto.SetVersion(m, c.ProtocolVersion)
	if err := c.sync(m); err != nil {
		c.releaseid(id)
		return nil
	}
	select {
	case ack := <-c.suback:
		return ack
	case <-c.closed:
		return nil
	}
}

// Unsubscribe removes the given topic filters and waits for UNSUBACK.
func (c *ClientConn) Unsubscribe(topics []string) *proto.UnsubAck {
	id, err := c.nextid()
	if err != nil {
		return nil
	}
	m := &proto.Unsubscribe{MessageId: id, Topics: topics}
	proto.SetVersion(m, c.ProtocolVersion)
	if err := c.sync(m); err != nil {
		c.releaseid(id)
		return nil
	}
	select {
	case ack := <-c.unsuback:
		return ack
	case <-c.closed:
		return nil
	}
}

// Publish publishes the given message to the MQTT server.
// QoS 0 and QoS 1 are always supported. QoS 2 requires EnableQoS2 in the
// ClientOptions used to connect.
func (c *ClientConn) Publish(m *proto.Publish) error {
	if m == nil {
		return fmt.Errorf("mqtt: nil publish")
	}
	if m.QosLevel > proto.QosExactlyOnce || (m.QosLevel == proto.QosExactlyOnce && !c.EnableQoS2) {
		return fmt.Errorf("mqtt: unsupported QoS level %d", m.QosLevel)
	}
	if m.QosLevel > c.maximumQoS {
		return fmt.Errorf("mqtt: QoS level %d exceeds server maximum %d", m.QosLevel, c.maximumQoS)
	}
	allocated := false
	if m.QosLevel.HasId() {
		id, err := c.nextid()
		if err != nil {
			return err
		}
		m.MessageId = id
		allocated = true
	}
	proto.SetVersion(m, c.ProtocolVersion)
	if m.QosLevel == proto.QosExactlyOnce {
		c.qosMu.Lock()
		c.outgoingQoS2[m.MessageId] = m
		c.qosMu.Unlock()
	}
	if err := c.sync(m); err != nil {
		if allocated {
			c.releaseid(m.MessageId)
		}
		if m.QosLevel == proto.QosExactlyOnce {
			c.qosMu.Lock()
			delete(c.outgoingQoS2, m.MessageId)
			c.qosMu.Unlock()
		}
		return err
	}
	return nil
}

// sync sends a message and blocks until it was actually sent.
func (c *ClientConn) sync(m proto.Message) (err error) {
	j := job{m: m, r: make(receipt, 1)}
	select {
	case c.out <- j:
	case <-c.closed:
		return ErrClientClosed
	}
	select {
	case err = <-j.r:
		return err
	case <-c.closed:
		// The peer can close immediately after receiving our packet (notably
		// DISCONNECT), before the writer publishes its successful receipt.
		// Wait for the writer to finish before deciding whether the send failed.
		<-c.done
		select {
		case err = <-j.r:
			return err
		default:
			return ErrClientClosed
		}
	}
}
