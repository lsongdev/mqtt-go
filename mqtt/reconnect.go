package mqtt

import (
	"context"
	"sync"
	"time"

	"github.com/lsongdev/mqtt-go/proto"
)

// ReconnectOptions controls the retry loop used by DialWithReconnect.
type ReconnectOptions struct {
	MinDelay    time.Duration
	MaxDelay    time.Duration
	DialTimeout time.Duration
}

func (o ReconnectOptions) normalized() ReconnectOptions {
	if o.MinDelay <= 0 {
		o.MinDelay = 250 * time.Millisecond
	}
	if o.MaxDelay <= 0 {
		o.MaxDelay = 30 * time.Second
	}
	if o.MaxDelay < o.MinDelay {
		o.MaxDelay = o.MinDelay
	}
	if o.DialTimeout <= 0 {
		o.DialTimeout = 10 * time.Second
	}
	return o
}

// ReconnectingClient owns a sequence of ClientConn values. ClientConn remains
// the one-transport primitive; this type adds redial, subscription replay, and
// a stable Incoming channel across reconnects.
type ReconnectingClient struct {
	address   string
	options   ClientOptions
	reconnect ReconnectOptions

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu            sync.RWMutex
	conn          *ClientConn
	subscriptions map[string]proto.TopicQos
	ops           sync.Mutex

	Incoming chan *proto.Publish
}

// DialWithReconnect establishes the first connection and then keeps it alive
// with exponential backoff after unexpected transport loss. ctx applies to the
// initial dial only; Close or Disconnect stops the reconnect loop.
func DialWithReconnect(ctx context.Context, address string, options ClientOptions, reconnect ReconnectOptions) (*ReconnectingClient, error) {
	conn, err := Dial(ctx, address, options)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	client := &ReconnectingClient{
		address:       address,
		options:       options.normalized(),
		reconnect:     reconnect.normalized(),
		ctx:           runCtx,
		cancel:        cancel,
		done:          make(chan struct{}),
		conn:          conn,
		subscriptions: make(map[string]proto.TopicQos),
		Incoming:      make(chan *proto.Publish, clientQueueLength),
	}
	// MQTT 5 may assign the ClientID during the first CONNECT. Reuse it for all
	// later Network Connections.
	client.options.ClientID = conn.ClientId
	go client.run(conn)
	return client, nil
}

// Connected reports whether a live MQTT transport is currently attached.
func (c *ReconnectingClient) Connected() bool {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return false
	}
	select {
	case <-conn.closed:
		return false
	default:
		return true
	}
}

// Done is closed after the reconnect loop has stopped.
func (c *ReconnectingClient) Done() <-chan struct{} { return c.done }

func (c *ReconnectingClient) current() *ClientConn {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.conn
}

func (c *ReconnectingClient) setCurrent(conn *ClientConn) {
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
}

func (c *ReconnectingClient) clearCurrent(conn *ClientConn) {
	c.mu.Lock()
	if c.conn == conn {
		c.conn = nil
	}
	c.mu.Unlock()
}

func (c *ReconnectingClient) run(conn *ClientConn) {
	defer close(c.done)
	defer close(c.Incoming)

	for {
		for message := range conn.Incoming {
			select {
			case c.Incoming <- message:
			case <-c.ctx.Done():
				_ = conn.Close()
				<-conn.done
				return
			}
		}
		<-conn.done
		c.clearCurrent(conn)

		select {
		case <-c.ctx.Done():
			return
		default:
		}

		delay := c.reconnect.MinDelay
		for {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-c.ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return
			}

			dialCtx, cancel := context.WithTimeout(c.ctx, c.reconnect.DialTimeout)
			next, err := Dial(dialCtx, c.address, c.options)
			cancel()
			if err == nil {
				if next.ClientId != "" {
					c.options.ClientID = next.ClientId
				}
				c.ops.Lock()
				if !next.SessionPresent {
					if !c.replaySubscriptions(next) {
						c.ops.Unlock()
						_ = next.Close()
						<-next.done
						err = ErrClientClosed
					} else {
						c.setCurrent(next)
						c.ops.Unlock()
						conn = next
						break
					}
				} else {
					c.setCurrent(next)
					c.ops.Unlock()
					conn = next
					break
				}
			}

			if delay < c.reconnect.MaxDelay {
				delay *= 2
				if delay > c.reconnect.MaxDelay {
					delay = c.reconnect.MaxDelay
				}
			}
		}
	}
}

func (c *ReconnectingClient) replaySubscriptions(conn *ClientConn) bool {
	c.mu.RLock()
	topics := make([]proto.TopicQos, 0, len(c.subscriptions))
	for _, topic := range c.subscriptions {
		topics = append(topics, topic)
	}
	c.mu.RUnlock()
	if len(topics) == 0 {
		return true
	}
	return conn.Subscribe(topics) != nil
}

// Publish sends using the current transport. It does not queue application
// messages while disconnected; callers retain control over retry semantics.
func (c *ReconnectingClient) Publish(message *proto.Publish) error {
	c.ops.Lock()
	defer c.ops.Unlock()
	conn := c.current()
	if conn == nil {
		return ErrClientClosed
	}
	return conn.Publish(message)
}

// Subscribe records successful subscriptions and replays them when a new
// broker Session is created after reconnect.
func (c *ReconnectingClient) Subscribe(topics []proto.TopicQos) *proto.SubAck {
	c.ops.Lock()
	defer c.ops.Unlock()
	conn := c.current()
	if conn == nil {
		return nil
	}
	ack := conn.Subscribe(topics)
	if ack == nil {
		return nil
	}
	c.mu.Lock()
	for i, topic := range topics {
		granted := true
		if conn.ProtocolVersion == proto.Version5 {
			granted = i < len(ack.ReasonCodes) && ack.ReasonCodes[i] < 0x80
		} else {
			granted = i < len(ack.TopicsQos) && ack.TopicsQos[i] != proto.QosLevel(0x80)
		}
		if granted {
			c.subscriptions[topic.Topic] = topic
		}
	}
	c.mu.Unlock()
	return ack
}

// Unsubscribe removes successful subscriptions from the replay set.
func (c *ReconnectingClient) Unsubscribe(filters []string) *proto.UnsubAck {
	c.ops.Lock()
	defer c.ops.Unlock()
	conn := c.current()
	if conn == nil {
		return nil
	}
	ack := conn.Unsubscribe(filters)
	if ack == nil {
		return nil
	}
	c.mu.Lock()
	for _, filter := range filters {
		delete(c.subscriptions, filter)
	}
	c.mu.Unlock()
	return ack
}

// Disconnect performs a graceful MQTT disconnect and disables reconnection.
func (c *ReconnectingClient) Disconnect() {
	c.cancel()
	c.ops.Lock()
	conn := c.current()
	if conn != nil {
		conn.Disconnect()
	}
	c.ops.Unlock()
	<-c.done
}

// Close immediately closes the current transport and disables reconnection.
func (c *ReconnectingClient) Close() error {
	c.cancel()
	c.ops.Lock()
	conn := c.current()
	var err error
	if conn != nil {
		err = conn.Close()
	}
	c.ops.Unlock()
	<-c.done
	return err
}
