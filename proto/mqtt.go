// Package proto implements streaming MQTT 3.1.1 and MQTT 5.0 packet encoding
// and decoding. Broker and client semantics live in the mqtt package.
//
// See http://public.dhe.ibm.com/software/dw/webservices/ws-mqtt/mqtt-v3r1.html
// for the MQTT protocol specification. This package does not implement the
// semantics of MQTT, but purely the encoding and decoding of its messages.
//
// Decoding Messages:
//
// Use the DecodeOneMessage function to read a Message from an io.Reader, it
// will return a Message value. The function can be implemented using the public
// API of this package if more control is required. For example:
//
//	for {
//	  msg, err := mqtt.DecodeOneMessage(conn, nil)
//	  if err != nil {
//	    // handle err
//	  }
//	  switch msg := msg.(type) {
//	  case *Connect:
//	    // ...
//	  case *Publish:
//	    // ...
//	    // etc.
//	  }
//	}
//
// Encoding Messages:
//
// Create a message value, and use its Encode method to write it to an
// io.Writer. For example:
//
//	someData := []byte{1, 2, 3}
//	msg := &Publish{
//	  Header: {
//	    DupFlag: false,
//	    QosLevel: QosAtLeastOnce,
//	    Retain: false,
//	  },
//	  TopicName: "a/b",
//	  MessageId: 10,
//	  Payload: BytesPayload(someData),
//	}
//	if err := msg.Encode(conn); err != nil {
//	  // handle err
//	}
//
// Advanced PUBLISH payload handling:
//
// The default behaviour for decoding PUBLISH payloads, and most common way to
// supply payloads for encoding, is the BytesPayload, which is a []byte
// derivative.
//
// More complex handling is possible by implementing the Payload interface,
// which can be injected into DecodeOneMessage via the `config` parameter, or
// into an outgoing Publish message via its Payload field.  Potential benefits
// of this include:
//
// * Data can be (un)marshalled directly on a connection, without an unecessary
// round-trip via bytes.Buffer.
//
// * Data can be streamed directly on readers/writers (e.g files, other
// connections, pipes) without the requirement to buffer an entire message
// payload in memory at once.
//
// The limitations of these streaming features are:
//
// * When encoding a payload, the encoded size of the payload must be known and
// declared upfront.
//
// * The payload size (and PUBLISH variable header) can be no more than 256MiB
// minus 1 byte. This is a specified limitation of MQTT v3.1 itself.
package proto

import (
	"errors"
	"io"
)

var (
	badMsgTypeError        = errors.New("mqtt: message type is invalid")
	badQosError            = errors.New("mqtt: QoS is invalid")
	badWillQosError        = errors.New("mqtt: will QoS is invalid")
	badLengthEncodingError = errors.New("mqtt: remaining length field exceeded maximum of 4 bytes")
	badReturnCodeError     = errors.New("mqtt: is invalid")
	dataExceedsPacketError = errors.New("mqtt: data exceeds packet length")
	msgTooLongError        = errors.New("mqtt: message is too long")
	badPacketIdentifierError = errors.New("mqtt: packet identifier must be non-zero")

	// ErrPacketTooLarge is returned when a decoder packet-size limit is exceeded.
	ErrPacketTooLarge = errors.New("mqtt: packet exceeds configured maximum size")
)

const (
	QosAtMostOnce = QosLevel(iota)
	QosAtLeastOnce
	QosExactlyOnce

	qosFirstInvalid
)

type QosLevel uint8

func (qos QosLevel) IsValid() bool {
	return qos < qosFirstInvalid
}

func (qos QosLevel) HasId() bool {
	return qos == QosAtLeastOnce || qos == QosExactlyOnce
}

const (
	RetCodeAccepted                    = ReturnCode(0)
	RetCodeUnacceptableProtocolVersion = ReturnCode(1)
	RetCodeIdentifierRejected          = ReturnCode(2)
	RetCodeServerUnavailable           = ReturnCode(3)
	RetCodeBadUsernameOrPassword       = ReturnCode(4)
	RetCodeNotAuthorized               = ReturnCode(5)
	RetCodeMalformedPacket             = ReturnCode(6)
	RetCodeProtocolError               = ReturnCode(7)

	retCodeFirstInvalid
)

type ReturnCode uint8

func (rc ReturnCode) IsValid() bool {
	return rc >= RetCodeAccepted && rc <= RetCodeNotAuthorized
}

// DecoderConfig provides configuration for decoding messages.
type DecoderConfig interface {
	// MakePayload returns a Payload for the given Publish message. r is a Reader
	// that will read the payload data, and n is the number of bytes in the
	// payload. The Payload.ReadPayload method is called on the returned payload
	// by the decoding process.
	MakePayload(msg *Publish, r io.Reader, n int) (Payload, error)
}

// VersionedDecoderConfig optionally tells the decoder which wire format to
// use after CONNECT. MQTT 5 adds a properties field to most packets, so the
// protocol level must be carried by the connection.
type VersionedDecoderConfig interface {
	DecoderConfig
	MQTTVersion() ProtocolVersion
}

// DecodeOptions is the standard decoder configuration. Version defaults to
// Version311 when left unset, preserving the behaviour of older callers.
type DecodeOptions struct {
	Version        ProtocolVersion
	// MaxPacketSize limits the MQTT Remaining Length accepted by the decoder.
	// Zero keeps the protocol maximum. The check happens before payload allocation.
	MaxPacketSize  int
	PayloadFactory func(*Publish, io.Reader, int) (Payload, error)
}

func (o *DecodeOptions) MQTTVersion() ProtocolVersion {
	if o == nil || o.Version == 0 {
		return Version311
	}
	return o.Version
}

func (o *DecodeOptions) MakePayload(msg *Publish, r io.Reader, n int) (Payload, error) {
	if o != nil && o.PayloadFactory != nil {
		return o.PayloadFactory(msg, r, n)
	}
	return make(BytesPayload, n), nil
}

func decoderPacketLimit(c DecoderConfig) int {
	if o, ok := c.(*DecodeOptions); ok && o != nil {
		return o.MaxPacketSize
	}
	return 0
}

func decoderVersion(c DecoderConfig) ProtocolVersion {
	if c, ok := c.(VersionedDecoderConfig); ok {
		return c.MQTTVersion()
	}
	return Version311
}

type DefaultDecoderConfig struct{}

func (c DefaultDecoderConfig) MakePayload(msg *Publish, r io.Reader, n int) (Payload, error) {
	return make(BytesPayload, n), nil
}

// ValueConfig always returns the given Payload when MakePayload is called.
type ValueConfig struct {
	Payload Payload
}

func (c *ValueConfig) MakePayload(msg *Publish, r io.Reader, n int) (Payload, error) {
	return c.Payload, nil
}

// DecodeOneMessage decodes one message from r. config provides specifics on
// how to decode messages, nil indicates that the DefaultDecoderConfig should
// be used.
func DecodeOneMessage(r io.Reader, config DecoderConfig) (msg Message, err error) {
	var hdr Header
	var msgType MessageType
	var packetRemaining int32
	msgType, packetRemaining, err = hdr.Decode(r)
	if err != nil {
		return
	}
	if err = validateFixedHeader(msgType, hdr); err != nil {
		return nil, err
	}

	msg, err = NewMessage(msgType)
	if err != nil {
		return
	}

	if config == nil {
		config = DefaultDecoderConfig{}
	}
	if limit := decoderPacketLimit(config); limit > 0 && int64(packetRemaining) > int64(limit) {
		return nil, ErrPacketTooLarge
	}

	return msg, msg.Decode(r, hdr, packetRemaining, config)
}

func validateFixedHeader(mt MessageType, h Header) error {
	if mt == MsgPublish {
		if h.QosLevel == qosFirstInvalid || (h.QosLevel == QosAtMostOnce && h.DupFlag) {
			return badQosError
		}
		return nil
	}
	if mt == MsgPubRel || mt == MsgSubscribe || mt == MsgUnsubscribe {
		if !h.DupFlag && h.QosLevel == QosAtLeastOnce && !h.Retain {
			return nil
		}
		return errors.New("mqtt: invalid fixed header flags")
	}
	if h.DupFlag || h.QosLevel != QosAtMostOnce || h.Retain {
		return errors.New("mqtt: invalid fixed header flags")
	}
	return nil
}

// NewMessage creates an instance of a Message value for the given message
// type. An error is returned if msgType is invalid.
func NewMessage(msgType MessageType) (msg Message, err error) {
	switch msgType {
	case MsgConnect:
		msg = new(Connect)
	case MsgConnAck:
		msg = new(ConnAck)
	case MsgPublish:
		msg = new(Publish)
	case MsgPubAck:
		msg = new(PubAck)
	case MsgPubRec:
		msg = new(PubRec)
	case MsgPubRel:
		msg = new(PubRel)
	case MsgPubComp:
		msg = new(PubComp)
	case MsgSubscribe:
		msg = new(Subscribe)
	case MsgUnsubAck:
		msg = new(UnsubAck)
	case MsgSubAck:
		msg = new(SubAck)
	case MsgUnsubscribe:
		msg = new(Unsubscribe)
	case MsgPingReq:
		msg = new(PingReq)
	case MsgPingResp:
		msg = new(PingResp)
	case MsgDisconnect:
		msg = new(Disconnect)
	case MsgAuth:
		msg = new(Auth)
	default:
		return nil, badMsgTypeError
	}

	return
}

// SetVersion marks a packet for the requested wire format. CONNECT carries
// its own protocol level; all other MQTT 5 packets need this connection-level
// context when encoded.
func SetVersion(msg Message, version ProtocolVersion) {
	switch m := msg.(type) {
	case *ConnAck:
		m.Header.Version = version
	case *Publish:
		m.Header.Version = version
	case *PubAck:
		m.Header.Version = version
	case *PubRec:
		m.Header.Version = version
	case *PubRel:
		m.Header.Version = version
	case *PubComp:
		m.Header.Version = version
	case *Subscribe:
		m.Header.Version = version
	case *SubAck:
		m.Header.Version = version
	case *Unsubscribe:
		m.Header.Version = version
	case *UnsubAck:
		m.Header.Version = version
	case *PingReq:
		m.Header.Version = version
	case *PingResp:
		m.Header.Version = version
	case *Disconnect:
		m.Header.Version = version
	case *Auth:
		m.Header.Version = version
	}
}

// panicErr wraps an error that caused a problem that needs to bail out of the
// API, such that errors can be recovered and returned as errors from the
// public API.
type panicErr struct {
	err error
}

func (p panicErr) Error() string {
	return p.err.Error()
}

func raiseError(err error) {
	panic(panicErr{err})
}

// recoverError recovers any panic in flight and, iff it's an error from
// raiseError, will return the error. Otherwise re-raises the panic value.
// If no panic is in flight, it returns existingErr.
//
// This must be used in combination with a defer in all public API entry
// points where raiseError could be called.
func recoverError(existingErr error, recovered interface{}) error {
	if recovered != nil {
		if pErr, ok := recovered.(panicErr); ok {
			return pErr.err
		} else {
			panic(recovered)
		}
	}
	return existingErr
}
