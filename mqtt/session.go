package mqtt

import (
	"bytes"
	"context"
	"errors"
	"log"
	"time"

	"github.com/lsongdev/mqtt-go/proto"
)

// ErrSessionNotFound is returned when a persistent session does not exist.
var ErrSessionNotFound = errors.New("mqtt: session not found")

// StoredSubscription is the durable form of a topic subscription.
type StoredSubscription struct {
	Filter            string
	QoS               proto.QosLevel
	NoLocal           bool
	RetainAsPublished bool
	RetainHandling    byte
}

// StoredMessage is a durable queued or unacknowledged application message.
// PacketID and Stage allow persistent QoS delivery to resume after reconnect.
type StoredMessage struct {
	ID         uint64
	PacketID   uint16
	Stage      byte // 0: PUBLISH, 1: PUBREL (QoS 2 only).
	Topic      string
	QoS        proto.QosLevel
	Retain     bool
	Payload    []byte
	Properties proto.Properties
}

// StoredSession is the complete broker state that survives a disconnect.
type StoredSession struct {
	ClientID      string
	ExpiresAt     time.Time // Zero means no expiry.
	Subscriptions []StoredSubscription
	Queue         []StoredMessage
}

// SessionStore is the optional persistence boundary used by Server. Stores
// must be safe for concurrent use.
type SessionStore interface {
	List(context.Context) ([]StoredSession, error)
	Save(context.Context, StoredSession) error
	Delete(context.Context, string) error
	Close() error
}

type sessionState struct {
	StoredSession
	conn           *incomingConn
	nextQueueID    uint64
	server         *Server
	expiryInterval uint32 // MQTT 5 lifetime after transport disconnects.
}

func propertyUint32(properties proto.Properties, id proto.PropertyID) (uint32, bool) {
	for _, p := range properties {
		if p.ID == id {
			value, ok := p.Value.(uint32)
			return value, ok
		}
	}
	return 0, false
}

func propertyUint16(properties proto.Properties, id proto.PropertyID) (uint16, bool) {
	for _, p := range properties {
		if p.ID == id {
			value, ok := p.Value.(uint16)
			return value, ok
		}
	}
	return 0, false
}

func (s *Server) attachSession(c *incomingConn, connect *proto.Connect) bool {
	enabled := s.persistentSessionsEnabled()
	persistent := enabled && !connect.CleanSession
	var expiryInterval uint32
	if c.version == proto.Version5 {
		seconds, _ := propertyUint32(connect.Properties, proto.PropertySessionExpiryInterval)
		persistent = enabled && seconds > 0
		expiryInterval = seconds
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if connect.CleanSession {
		s.deleteSessionLocked(connect.ClientId)
	}
	if !enabled {
		s.deleteSessionLocked(connect.ClientId)
	}
	state, present := s.sessions[connect.ClientId]
	if present && !state.ExpiresAt.IsZero() && !state.ExpiresAt.After(time.Now()) {
		s.deleteSessionLocked(connect.ClientId)
		state = nil
		present = false
	}
	if state == nil {
		state = &sessionState{StoredSession: StoredSession{ClientID: connect.ClientId}, server: s}
	}
	state.server = s
	// A connected Session never expires. Its expiry clock starts only after
	// the Network Connection closes, including when a resumed Session closes.
	state.ExpiresAt = time.Time{}
	state.expiryInterval = expiryInterval
	state.conn = c
	c.session = state
	c.persistent = persistent
	if persistent || present {
		s.sessions[connect.ClientId] = state
	}
	s.subs.bind(connect.ClientId, c)
	return present && !connect.CleanSession
}

func (s *Server) deleteSessionLocked(clientID string) {
	delete(s.sessions, clientID)
	s.subs.removeClient(clientID)
	if s.options.SessionStore != nil {
		if err := s.options.SessionStore.Delete(context.Background(), clientID); err != nil {
			log.Printf("mqtt: delete session %q: %v", clientID, err)
		}
	}
}

func (s *Server) saveSession(state *sessionState) {
	if state == nil || s.options.SessionStore == nil {
		return
	}
	if err := s.options.SessionStore.Save(context.Background(), state.StoredSession); err != nil {
		log.Printf("mqtt: save session %q: %v", state.ClientID, err)
	}
}

func (s *Server) detachSession(c *incomingConn) {
	if c.session == nil {
		s.subs.detach(c, false)
		return
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if c.persistent {
		c.session.conn = nil
		if c.version == proto.Version5 && c.session.expiryInterval != ^uint32(0) {
			c.session.ExpiresAt = time.Now().Add(time.Duration(c.session.expiryInterval) * time.Second)
		}
		s.sessions[c.clientid] = c.session
		s.subs.detach(c, true)
		s.saveSession(c.session)
	} else {
		if s.sessions[c.clientid] == c.session {
			s.deleteSessionLocked(c.clientid)
		} else {
			s.subs.detach(c, false)
		}
	}
}

func (s *Server) updateSessionExpiry(c *incomingConn, properties proto.Properties) {
	if c.version != proto.Version5 || c.session == nil {
		return
	}
	seconds, ok := propertyUint32(properties, proto.PropertySessionExpiryInterval)
	if !ok {
		return
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	c.persistent = s.persistentSessionsEnabled() && seconds > 0
	c.session.expiryInterval = seconds
}

func (s *Server) recordSubscription(c *incomingConn, tq proto.TopicQos) {
	if c.session == nil {
		return
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	stored := StoredSubscription{Filter: tq.Topic, QoS: tq.Qos, NoLocal: tq.NoLocal, RetainAsPublished: tq.RetainAsPublished, RetainHandling: tq.RetainHandling}
	found := false
	for i := range c.session.Subscriptions {
		if c.session.Subscriptions[i].Filter == tq.Topic {
			c.session.Subscriptions[i] = stored
			found = true
			break
		}
	}
	if !found {
		c.session.Subscriptions = append(c.session.Subscriptions, stored)
	}
	if c.persistent {
		s.saveSession(c.session)
	}
}

func (s *Server) removeSubscription(c *incomingConn, filter string) {
	if c.session == nil {
		return
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	out := c.session.Subscriptions[:0]
	for _, sub := range c.session.Subscriptions {
		if sub.Filter != filter {
			out = append(out, sub)
		}
	}
	c.session.Subscriptions = out
	if c.persistent {
		s.saveSession(c.session)
	}
}

func (s *Server) queueSession(state *sessionState, message StoredMessage) uint64 {
	if state == nil {
		return 0
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	state.nextQueueID++
	message.ID = state.nextQueueID
	state.Queue = append(state.Queue, message)
	s.saveSession(state)
	return message.ID
}

func (s *Server) markSessionDelivery(state *sessionState, id uint64, packetID uint16, stage byte) {
	if state == nil || id == 0 {
		return
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	for i := range state.Queue {
		if state.Queue[i].ID == id {
			state.Queue[i].PacketID = packetID
			state.Queue[i].Stage = stage
			break
		}
	}
	s.saveSession(state)
}

func (s *Server) sessionActive(state *sessionState) bool {
	if state == nil {
		return false
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if !state.ExpiresAt.IsZero() && !state.ExpiresAt.After(time.Now()) {
		s.deleteSessionLocked(state.ClientID)
		return false
	}
	return true
}

func (s *Server) ackSession(state *sessionState, id uint64) {
	if state == nil || id == 0 {
		return
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	out := state.Queue[:0]
	for _, m := range state.Queue {
		if m.ID != id {
			out = append(out, m)
		}
	}
	state.Queue = out
	if state.conn != nil || state.ExpiresAt.IsZero() || state.ExpiresAt.After(time.Now()) {
		s.saveSession(state)
	}
}

func boolToProperty(v bool) byte {
	if v {
		return 1
	}
	return 0
}

func (c *incomingConn) deliverSessionQueue() {
	if c.session == nil {
		return
	}
	c.svr.sessionsMu.Lock()
	queued := append([]StoredMessage(nil), c.session.Queue...)
	c.svr.sessionsMu.Unlock()
	for _, stored := range queued {
		if stored.PacketID != 0 {
			c.reserveMessageID(stored.PacketID)
		}
		if stored.Stage == 1 && stored.QoS == proto.QosExactlyOnce && stored.PacketID != 0 {
			message := stored.publish()
			message.MessageId = stored.PacketID
			c.qosMu.Lock()
			c.outgoingQoS2[stored.PacketID] = message
			c.outgoingStored[stored.PacketID] = stored.ID
			c.qosMu.Unlock()
			c.submit(&proto.PubRel{MessageId: stored.PacketID})
			continue
		}
		message := stored.publish()
		if message.QosLevel == proto.QosExactlyOnce && !c.svr.options.EnableQoS2 {
			message.QosLevel = proto.QosAtLeastOnce
		}
		message.MessageId = stored.PacketID
		message.DupFlag = stored.PacketID != 0
		if message.QosLevel.HasId() && message.MessageId == 0 {
			message.MessageId = c.nextMessageID()
			if message.MessageId == 0 {
				return
			}
			c.svr.markSessionDelivery(c.session, stored.ID, message.MessageId, 0)
		}
		c.trackAndSubmit(message, stored.ID)
	}
}

func (c *incomingConn) trackAndSubmit(message *proto.Publish, storedID uint64) {
	if message.QosLevel.HasId() {
		if message.MessageId == 0 {
			message.MessageId = c.nextMessageID()
			if message.MessageId == 0 {
				return
			}
		} else {
			c.reserveMessageID(message.MessageId)
		}
	}
	c.qosMu.Lock()
	if message.QosLevel == proto.QosExactlyOnce {
		c.outgoingQoS2[message.MessageId] = message
	}
	if storedID != 0 {
		c.outgoingStored[message.MessageId] = storedID
	}
	c.qosMu.Unlock()
	c.submit(message)
}

func (c *incomingConn) ackOutgoing(packetID uint16) {
	c.qosMu.Lock()
	storedID := c.outgoingStored[packetID]
	delete(c.outgoingStored, packetID)
	c.qosMu.Unlock()
	if storedID != 0 {
		c.svr.ackSession(c.session, storedID)
	}
	select {
	case c.publishAcks <- packetID:
	case <-c.closed:
	default:
		c.stop()
	}
	c.releaseMessageID(packetID)
}

func storedPublish(m *proto.Publish) (StoredMessage, bool) {
	var payload bytes.Buffer
	if m.Payload != nil {
		if err := m.Payload.WritePayload(&payload); err != nil {
			return StoredMessage{}, false
		}
	}
	return StoredMessage{PacketID: m.MessageId, Topic: m.TopicName, QoS: m.QosLevel, Retain: m.Retain, Payload: payload.Bytes(), Properties: append(proto.Properties(nil), m.Properties...)}, true
}

func (m StoredMessage) publish() *proto.Publish {
	return &proto.Publish{Header: proto.Header{QosLevel: m.QoS, Retain: m.Retain}, TopicName: m.Topic, Payload: proto.BytesPayload(append([]byte(nil), m.Payload...)), Properties: append(proto.Properties(nil), m.Properties...)}
}
