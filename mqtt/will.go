package mqtt

import (
	"time"

	"github.com/lsongdev/mqtt-go/proto"
)

// Will configures the application message published when a client connection
// ends without a normal DISCONNECT.
type Will struct {
	Topic      string
	Payload    []byte
	QoS        proto.QosLevel
	Retain     bool
	Properties proto.Properties
}

type pendingWill struct {
	message *proto.Publish
	timer   *time.Timer
}

func willDelay(properties proto.Properties) time.Duration {
	if seconds, ok := propertyUint32(properties, proto.PropertyWillDelayInterval); ok {
		return time.Duration(seconds) * time.Second
	}
	return 0
}

func publishProperties(properties proto.Properties) proto.Properties {
	out := make(proto.Properties, 0, len(properties))
	for _, p := range properties {
		if p.ID != proto.PropertyWillDelayInterval {
			out = append(out, p)
		}
	}
	return out
}

func (c *incomingConn) configureWill(connect *proto.Connect) {
	if !connect.WillFlag {
		return
	}
	c.will = &proto.Publish{
		Header: proto.Header{
			Version:  c.version,
			QosLevel: connect.WillQos,
			Retain:   connect.WillRetain,
		},
		TopicName:  connect.WillTopic,
		Payload:    proto.BytesPayload([]byte(connect.WillMessage)),
		Properties: publishProperties(connect.WillProperties),
	}
	c.willDelay = willDelay(connect.WillProperties)
	c.willArmed = true
}

func (c *incomingConn) discardWill() {
	c.willMu.Lock()
	c.willArmed = false
	c.will = nil
	c.willMu.Unlock()
}

func (c *incomingConn) takeWill() (*proto.Publish, time.Duration) {
	c.willMu.Lock()
	defer c.willMu.Unlock()
	if !c.willArmed || c.will == nil {
		return nil, 0
	}
	c.willArmed = false
	message := c.will
	c.will = nil

	delay := c.willDelay
	if c.version == proto.Version5 {
		// A Will is published when its delay expires or the Session ends,
		// whichever occurs first.
		if !c.persistent || c.session == nil {
			delay = 0
		} else if !c.session.ExpiresAt.IsZero() {
			remaining := time.Until(c.session.ExpiresAt)
			if remaining <= 0 {
				delay = 0
			} else if delay == 0 || remaining < delay {
				delay = remaining
			}
		}
	}
	return message, delay
}

func (s *Server) scheduleWill(c *incomingConn) {
	message, delay := c.takeWill()
	if message == nil {
		return
	}
	if delay <= 0 {
		s.subs.submit(nil, message)
		return
	}

	pending := &pendingWill{message: message}
	pending.timer = time.AfterFunc(delay, func() {
		s.willsMu.Lock()
		if s.wills[c.clientid] != pending {
			s.willsMu.Unlock()
			return
		}
		delete(s.wills, c.clientid)
		s.willsMu.Unlock()
		s.subs.submit(nil, message)
	})
	s.willsMu.Lock()
	if previous := s.wills[c.clientid]; previous != nil {
		previous.timer.Stop()
	}
	s.wills[c.clientid] = pending
	s.willsMu.Unlock()
}

// resolvePendingWill handles a new CONNECT for the same ClientID. Resuming the
// existing Session suppresses a delayed Will; Clean Start ends the old Session
// and therefore publishes it immediately.
func (s *Server) resolvePendingWill(clientID string, cleanStart bool) {
	s.willsMu.Lock()
	pending := s.wills[clientID]
	if pending != nil {
		delete(s.wills, clientID)
		if pending.timer != nil {
			pending.timer.Stop()
		}
	}
	s.willsMu.Unlock()
	if pending != nil && cleanStart {
		s.subs.submit(nil, pending.message)
	}
}
