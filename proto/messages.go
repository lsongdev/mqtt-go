package proto

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	// Maximum payload size in bytes (256MiB - 1B).
	MaxPayloadSize = (1 << (4 * 7)) - 1

	PROTOCOL_3_1   = "MQIsdp"
	PROTOCOL_3_1_1 = "MQTT"
	PROTOCOL_5_0   = "MQTT"
)

// ProtocolVersion is the MQTT protocol level placed in CONNECT.
type ProtocolVersion uint8
type ReasonCode uint8

const (
	Version311 ProtocolVersion = 4
	Version5   ProtocolVersion = 5
)

// Header contains the common attributes of all messages. Some attributes are
// not applicable to some message types.
type Header struct {
	DupFlag, Retain bool
	QosLevel        QosLevel
	// Version selects the wire format for packets other than CONNECT. The zero
	// value means MQTT 3.1.1 for backwards compatibility.
	Version ProtocolVersion
}

func (hdr Header) protocolVersion() ProtocolVersion {
	if hdr.Version == 0 {
		return Version311
	}
	return hdr.Version
}

func (hdr *Header) Encode(w io.Writer, msgType MessageType, remainingLength int32) error {
	buf := new(bytes.Buffer)
	err := hdr.encodeInto(buf, msgType, remainingLength)
	if err != nil {
		return err
	}
	_, err = w.Write(buf.Bytes())
	return err
}

func (hdr *Header) encodeInto(buf *bytes.Buffer, msgType MessageType, remainingLength int32) error {
	if !hdr.QosLevel.IsValid() {
		return badQosError
	}
	if !msgType.IsValid() {
		return badMsgTypeError
	}
	if err := validateFixedHeader(msgType, *hdr); err != nil {
		return err
	}

	val := byte(msgType) << 4
	val |= (boolToByte(hdr.DupFlag) << 3)
	val |= byte(hdr.QosLevel) << 1
	val |= boolToByte(hdr.Retain)
	buf.WriteByte(val)
	encodeLength(remainingLength, buf)
	return nil
}

func (hdr *Header) Decode(r io.Reader) (msgType MessageType, remainingLength int32, err error) {
	defer func() {
		err = recoverError(err, recover())
	}()

	var buf [1]byte

	if _, err = io.ReadFull(r, buf[:]); err != nil {
		return
	}

	byte1 := buf[0]
	msgType = MessageType((byte1 & 0xF0) >> 4)

	*hdr = Header{
		DupFlag:  byte1&0x08 > 0,
		QosLevel: QosLevel((byte1 & 0x06) >> 1),
		Retain:   byte1&0x01 > 0,
	}

	remainingLength = decodeLength(r)

	return
}

// Message is the interface that all MQTT messages implement.
type Message interface {
	// Encode writes the message to w.
	Encode(w io.Writer) error

	// Decode reads the message extended headers and payload from
	// r. Typically the values for hdr and packetRemaining will
	// be returned from Header.Decode.
	Decode(r io.Reader, hdr Header, packetRemaining int32, config DecoderConfig) error
}

// MessageType constants.
const (
	MsgConnect = MessageType(iota + 1)
	MsgConnAck
	MsgPublish
	MsgPubAck
	MsgPubRec
	MsgPubRel
	MsgPubComp
	MsgSubscribe
	MsgSubAck
	MsgUnsubscribe
	MsgUnsubAck
	MsgPingReq
	MsgPingResp
	MsgDisconnect
	MsgAuth

	msgTypeFirstInvalid
)

type MessageType uint8

// IsValid returns true if the MessageType value is valid.
func (mt MessageType) IsValid() bool {
	return mt >= MsgConnect && mt < msgTypeFirstInvalid
}

func writeMessage(w io.Writer, msgType MessageType, hdr *Header, payloadBuf *bytes.Buffer, extraLength int32) error {
	totalPayloadLength := int64(len(payloadBuf.Bytes())) + int64(extraLength)
	if totalPayloadLength > MaxPayloadSize {
		return msgTooLongError
	}

	buf := new(bytes.Buffer)
	err := hdr.encodeInto(buf, msgType, int32(totalPayloadLength))
	if err != nil {
		return err
	}

	buf.Write(payloadBuf.Bytes())
	_, err = w.Write(buf.Bytes())

	return err
}

// Connect represents an MQTT CONNECT message.
type Connect struct {
	Header
	ProtocolName               string
	ProtocolVersion            uint8
	WillRetain                 bool
	WillFlag                   bool
	CleanSession               bool
	WillQos                    QosLevel
	KeepAliveTimer             uint16
	ClientId                   string
	WillTopic, WillMessage     string
	UsernameFlag, PasswordFlag bool
	Username, Password         string
	ReservedBit                byte       // Added from 3.1.1
	Properties                 Properties // MQTT 5 CONNECT properties.
	WillProperties             Properties // MQTT 5 will properties.
}

func (msg *Connect) Encode(w io.Writer) (err error) {
	if !msg.WillQos.IsValid() {
		return badWillQosError
	}
	if err := validateUTF8String(msg.ClientId); err != nil {
		return err
	}
	if msg.UsernameFlag {
		if err := validateUTF8String(msg.Username); err != nil {
			return err
		}
	}
	if msg.PasswordFlag {
		if err := validateLengthPrefixed(len(msg.Password)); err != nil {
			return err
		}
	}
	if msg.WillFlag {
		if err := validateTopicName(msg.WillTopic, false); err != nil {
			return err
		}
		if err := validateLengthPrefixed(len(msg.WillMessage)); err != nil {
			return err
		}
	}
	if err := validateProtocolVersion(msg); err != nil {
		return err
	}

	buf := new(bytes.Buffer)

	flags := boolToByte(msg.UsernameFlag) << 7
	flags |= boolToByte(msg.PasswordFlag) << 6
	flags |= boolToByte(msg.WillRetain) << 5
	flags |= byte(msg.WillQos) << 3
	flags |= boolToByte(msg.WillFlag) << 2
	flags |= boolToByte(msg.CleanSession) << 1

	setString(msg.ProtocolName, buf)
	setUint8(msg.ProtocolVersion, buf)
	buf.WriteByte(flags)
	setUint16(msg.KeepAliveTimer, buf)
	if ProtocolVersion(msg.ProtocolVersion) == Version5 {
		if err := encodeProperties(buf, msg.Properties, propertiesConnect); err != nil {
			return err
		}
	}
	setString(msg.ClientId, buf)
	if msg.WillFlag {
		if ProtocolVersion(msg.ProtocolVersion) == Version5 {
			if err := encodeProperties(buf, msg.WillProperties, propertiesWill); err != nil {
				return err
			}
		}
		setString(msg.WillTopic, buf)
		setBinary([]byte(msg.WillMessage), buf)
	}
	if msg.UsernameFlag {
		setString(msg.Username, buf)
	}
	if msg.PasswordFlag {
		setBinary([]byte(msg.Password), buf)
	}

	return writeMessage(w, MsgConnect, &msg.Header, buf, 0)
}

func (msg *Connect) Decode(r io.Reader, hdr Header, packetRemaining int32, config DecoderConfig) (err error) {
	defer func() {
		err = recoverError(err, recover())
	}()

	msg.Header = hdr

	protocolName := getString(r, &packetRemaining)
	protocolVersion := getUint8(r, &packetRemaining)
	flags := getUint8(r, &packetRemaining)
	keepAliveTimer := getUint16(r, &packetRemaining)
	*msg = Connect{
		Header:          Header{Version: ProtocolVersion(protocolVersion)},
		ProtocolName:    protocolName,
		ProtocolVersion: protocolVersion,
		UsernameFlag:    flags&0x80 > 0,
		PasswordFlag:    flags&0x40 > 0,
		WillRetain:      flags&0x20 > 0,
		WillQos:         QosLevel(flags & 0x18 >> 3),
		WillFlag:        flags&0x04 > 0,
		CleanSession:    flags&0x02 > 0,
		ReservedBit:     flags & 0x01,
		KeepAliveTimer:  keepAliveTimer,
	}
	if ProtocolVersion(protocolVersion) == Version5 {
		msg.Properties = decodeProperties(r, &packetRemaining, propertiesConnect)
	}
	msg.ClientId = getString(r, &packetRemaining)

	if msg.WillFlag {
		if ProtocolVersion(protocolVersion) == Version5 {
			msg.WillProperties = decodeProperties(r, &packetRemaining, propertiesWill)
		}
		msg.WillTopic = getString(r, &packetRemaining)
		msg.WillMessage = string(getBinary(r, &packetRemaining))
	}
	if msg.UsernameFlag {
		msg.Username = getString(r, &packetRemaining)
	}
	if msg.PasswordFlag {
		msg.Password = string(getBinary(r, &packetRemaining))
	}

	if packetRemaining != 0 {
		return msgTooLongError
	}

	if err = validateProtocolVersion(msg); err != nil {
		return err
	}

	return nil
}

// ConnAck represents an MQTT CONNACK message.
type ConnAck struct {
	Header
	SessionPresent bool
	ReturnCode     ReturnCode
	Properties     Properties
}

func (msg *ConnAck) Encode(w io.Writer) (err error) {
	if msg.Header.protocolVersion() != Version5 && !msg.ReturnCode.IsValid() {
		return badReturnCodeError
	}
	if msg.SessionPresent && msg.ReturnCode != RetCodeAccepted {
		return errors.New("mqtt: session present requires successful CONNACK")
	}
	buf := new(bytes.Buffer)

	flags := 0x1 & boolToByte(msg.SessionPresent)
	buf.WriteByte(flags)
	setUint8(uint8(msg.ReturnCode), buf)
	if msg.Header.protocolVersion() == Version5 {
		if err := encodeProperties(buf, msg.Properties, propertiesConnAck); err != nil {
			return err
		}
	}

	return writeMessage(w, MsgConnAck, &msg.Header, buf, 0)
}

func (msg *ConnAck) Decode(r io.Reader, hdr Header, packetRemaining int32, config DecoderConfig) (err error) {
	defer func() {
		err = recoverError(err, recover())
	}()

	msg.Header = hdr
	msg.Header.Version = decoderVersion(config)

	msg.SessionPresent = (getUint8(r, &packetRemaining) & 0x01) > 0
	msg.ReturnCode = ReturnCode(getUint8(r, &packetRemaining))
	if msg.SessionPresent && msg.ReturnCode != RetCodeAccepted {
		return errors.New("mqtt: session present requires successful CONNACK")
	}
	if msg.Header.protocolVersion() != Version5 && !msg.ReturnCode.IsValid() {
		return badReturnCodeError
	}
	if msg.Header.protocolVersion() == Version5 {
		msg.Properties = decodeProperties(r, &packetRemaining, propertiesConnAck)
	}

	if packetRemaining != 0 {
		return msgTooLongError
	}

	return nil
}

// Publish represents an MQTT PUBLISH message.
type Publish struct {
	Header
	TopicName  string
	MessageId  uint16
	Payload    Payload
	Properties Properties
}

func (msg *Publish) Encode(w io.Writer) (err error) {
	if msg.Header.QosLevel.HasId() && msg.MessageId == 0 {
		return badPacketIdentifierError
	}
	allowEmptyTopic := msg.Header.protocolVersion() == Version5 && hasProperty(msg.Properties, PropertyTopicAlias)
	if err := validateTopicName(msg.TopicName, allowEmptyTopic); err != nil {
		return err
	}
	buf := new(bytes.Buffer)
	payload := msg.Payload
	if payload == nil {
		payload = BytesPayload(nil)
	}

	setString(msg.TopicName, buf)
	if msg.Header.QosLevel.HasId() {
		setUint16(msg.MessageId, buf)
	}
	if msg.Header.protocolVersion() == Version5 {
		if err = encodeProperties(buf, msg.Properties, propertiesPublish); err != nil {
			return err
		}
	}

	if err = writeMessage(w, MsgPublish, &msg.Header, buf, int32(payload.Size())); err != nil {
		return
	}

	return payload.WritePayload(w)
}

func (msg *Publish) Decode(r io.Reader, hdr Header, packetRemaining int32, config DecoderConfig) (err error) {
	defer func() {
		err = recoverError(err, recover())
	}()

	msg.Header = hdr
	msg.Header.Version = decoderVersion(config)

	msg.TopicName = getString(r, &packetRemaining)
	if msg.Header.QosLevel.HasId() {
		msg.MessageId = getUint16(r, &packetRemaining)
		if msg.MessageId == 0 {
			return badPacketIdentifierError
		}
	}
	if msg.Header.protocolVersion() == Version5 {
		msg.Properties = decodeProperties(r, &packetRemaining, propertiesPublish)
	}
	allowEmptyTopic := msg.Header.protocolVersion() == Version5 && hasProperty(msg.Properties, PropertyTopicAlias)
	if err := validateTopicName(msg.TopicName, allowEmptyTopic); err != nil {
		return err
	}

	payloadReader := &io.LimitedReader{R: r, N: int64(packetRemaining)}

	if msg.Payload, err = config.MakePayload(msg, payloadReader, int(packetRemaining)); err != nil {
		return
	}

	return msg.Payload.ReadPayload(payloadReader)
}

// PubAck represents an MQTT PUBACK message.
type PubAck struct {
	Header
	MessageId  uint16
	ReasonCode ReasonCode
	Properties Properties
}

func (msg *PubAck) Encode(w io.Writer) error {
	return encodeAckCommon(w, &msg.Header, msg.MessageId, msg.ReasonCode, msg.Properties, MsgPubAck)
}

func (msg *PubAck) Decode(r io.Reader, hdr Header, packetRemaining int32, config DecoderConfig) (err error) {
	msg.Header = hdr
	return decodeAckCommon(r, hdr, packetRemaining, &msg.MessageId, &msg.ReasonCode, &msg.Properties, config)
}

// PubRec represents an MQTT PUBREC message.
type PubRec struct {
	Header
	MessageId  uint16
	ReasonCode ReasonCode
	Properties Properties
}

func (msg *PubRec) Encode(w io.Writer) error {
	return encodeAckCommon(w, &msg.Header, msg.MessageId, msg.ReasonCode, msg.Properties, MsgPubRec)
}

func (msg *PubRec) Decode(r io.Reader, hdr Header, packetRemaining int32, config DecoderConfig) (err error) {
	msg.Header = hdr
	return decodeAckCommon(r, hdr, packetRemaining, &msg.MessageId, &msg.ReasonCode, &msg.Properties, config)
}

// PubRel represents an MQTT PUBREL message.
type PubRel struct {
	Header
	MessageId  uint16
	ReasonCode ReasonCode
	Properties Properties
}

func (msg *PubRel) Encode(w io.Writer) error {
	h := msg.Header
	h.QosLevel = QosAtLeastOnce
	return encodeAckCommon(w, &h, msg.MessageId, msg.ReasonCode, msg.Properties, MsgPubRel)
}

func (msg *PubRel) Decode(r io.Reader, hdr Header, packetRemaining int32, config DecoderConfig) (err error) {
	msg.Header = hdr
	return decodeAckCommon(r, hdr, packetRemaining, &msg.MessageId, &msg.ReasonCode, &msg.Properties, config)
}

// PubComp represents an MQTT PUBCOMP message.
type PubComp struct {
	Header
	MessageId  uint16
	ReasonCode ReasonCode
	Properties Properties
}

func (msg *PubComp) Encode(w io.Writer) error {
	return encodeAckCommon(w, &msg.Header, msg.MessageId, msg.ReasonCode, msg.Properties, MsgPubComp)
}

func (msg *PubComp) Decode(r io.Reader, hdr Header, packetRemaining int32, config DecoderConfig) (err error) {
	msg.Header = hdr
	return decodeAckCommon(r, hdr, packetRemaining, &msg.MessageId, &msg.ReasonCode, &msg.Properties, config)
}

// Subscribe represents an MQTT SUBSCRIBE message.
type Subscribe struct {
	Header
	MessageId  uint16
	Topics     []TopicQos
	Properties Properties
}

type TopicQos struct {
	Topic             string
	Qos               QosLevel
	NoLocal           bool
	RetainAsPublished bool
	RetainHandling    byte
}

func (msg *Subscribe) Encode(w io.Writer) (err error) {
	if msg.MessageId == 0 {
		return badPacketIdentifierError
	}
	if len(msg.Topics) == 0 {
		return errors.New("mqtt: SUBSCRIBE requires at least one topic filter")
	}
	buf := new(bytes.Buffer)
	setUint16(msg.MessageId, buf)
	if msg.Header.protocolVersion() == Version5 {
		if err := encodeProperties(buf, msg.Properties, propertiesSubscribe); err != nil {
			return err
		}
	}
	for _, topicSub := range msg.Topics {
		if err := validateTopicFilter(topicSub.Topic); err != nil {
			return err
		}
		if !topicSub.Qos.IsValid() || topicSub.RetainHandling > 2 {
			return errors.New("mqtt: invalid subscription options")
		}
		setString(topicSub.Topic, buf)
		options := uint8(topicSub.Qos)
		if msg.Header.protocolVersion() == Version5 {
			options |= boolToByte(topicSub.NoLocal)<<2 | boolToByte(topicSub.RetainAsPublished)<<3 | (topicSub.RetainHandling&3)<<4
		}
		setUint8(options, buf)
	}
	h := msg.Header
	h.QosLevel = QosAtLeastOnce
	return writeMessage(w, MsgSubscribe, &h, buf, 0)
}

func (msg *Subscribe) Decode(r io.Reader, hdr Header, packetRemaining int32, config DecoderConfig) (err error) {
	defer func() {
		err = recoverError(err, recover())
	}()

	msg.Header = hdr
	msg.Header.Version = decoderVersion(config)

	msg.MessageId = getUint16(r, &packetRemaining)
	if msg.MessageId == 0 {
		return badPacketIdentifierError
	}
	if msg.Header.protocolVersion() == Version5 {
		msg.Properties = decodeProperties(r, &packetRemaining, propertiesSubscribe)
	}
	var topics []TopicQos
	for packetRemaining > 0 {
		topic := getString(r, &packetRemaining)
		if err := validateTopicFilter(topic); err != nil {
			return err
		}
		options := getUint8(r, &packetRemaining)
		if options&0xc0 != 0 || QosLevel(options&3) == qosFirstInvalid || (msg.Header.protocolVersion() == Version5 && (options>>4)&3 == 3) || (msg.Header.protocolVersion() != Version5 && options&0xfc != 0) {
			return errors.New("mqtt: invalid subscription options")
		}
		topics = append(topics, TopicQos{
			Topic: topic, Qos: QosLevel(options & 3), NoLocal: options&4 != 0,
			RetainAsPublished: options&8 != 0, RetainHandling: (options >> 4) & 3,
		})
	}
	if len(topics) == 0 {
		return errors.New("mqtt: SUBSCRIBE requires at least one topic filter")
	}
	msg.Topics = topics

	return nil
}

// SubAck represents an MQTT SUBACK message.
type SubAck struct {
	Header
	MessageId   uint16
	TopicsQos   []QosLevel
	ReasonCodes []ReasonCode
	Properties  Properties
}

func (msg *SubAck) Encode(w io.Writer) (err error) {
	if msg.MessageId == 0 {
		return badPacketIdentifierError
	}
	buf := new(bytes.Buffer)
	setUint16(msg.MessageId, buf)
	if msg.Header.protocolVersion() == Version5 {
		if err := encodeProperties(buf, msg.Properties, propertiesSubAck); err != nil {
			return err
		}
		for _, reason := range msg.ReasonCodes {
			setUint8(uint8(reason), buf)
		}
	} else {
		for _, qos := range msg.TopicsQos {
			setUint8(uint8(qos), buf)
		}
	}

	return writeMessage(w, MsgSubAck, &msg.Header, buf, 0)
}

func (msg *SubAck) Decode(r io.Reader, hdr Header, packetRemaining int32, config DecoderConfig) (err error) {
	defer func() {
		err = recoverError(err, recover())
	}()

	msg.Header = hdr
	msg.Header.Version = decoderVersion(config)

	msg.MessageId = getUint16(r, &packetRemaining)
	if msg.MessageId == 0 {
		return badPacketIdentifierError
	}
	if msg.Header.protocolVersion() == Version5 {
		msg.Properties = decodeProperties(r, &packetRemaining, propertiesSubAck)
		for packetRemaining > 0 {
			msg.ReasonCodes = append(msg.ReasonCodes, ReasonCode(getUint8(r, &packetRemaining)))
		}
		return nil
	}
	topicsQos := make([]QosLevel, 0)
	for packetRemaining > 0 {
		grantedQos := QosLevel(getUint8(r, &packetRemaining))
		topicsQos = append(topicsQos, grantedQos)
	}
	msg.TopicsQos = topicsQos

	return nil
}

// Unsubscribe represents an MQTT UNSUBSCRIBE message.
type Unsubscribe struct {
	Header
	MessageId  uint16
	Topics     []string
	Properties Properties
}

func (msg *Unsubscribe) Encode(w io.Writer) (err error) {
	if msg.MessageId == 0 {
		return badPacketIdentifierError
	}
	if len(msg.Topics) == 0 {
		return errors.New("mqtt: UNSUBSCRIBE requires at least one topic filter")
	}
	buf := new(bytes.Buffer)
	setUint16(msg.MessageId, buf)
	if msg.Header.protocolVersion() == Version5 {
		if err := encodeProperties(buf, msg.Properties, propertiesUnsubscribe); err != nil {
			return err
		}
	}
	for _, topic := range msg.Topics {
		if err := validateTopicFilter(topic); err != nil {
			return err
		}
		setString(topic, buf)
	}

	h := msg.Header
	h.QosLevel = QosAtLeastOnce
	return writeMessage(w, MsgUnsubscribe, &h, buf, 0)
}

func (msg *Unsubscribe) Decode(r io.Reader, hdr Header, packetRemaining int32, config DecoderConfig) (err error) {
	defer func() {
		err = recoverError(err, recover())
	}()

	msg.Header = hdr
	msg.Header.Version = decoderVersion(config)

	msg.MessageId = getUint16(r, &packetRemaining)
	if msg.MessageId == 0 {
		return badPacketIdentifierError
	}
	if msg.Header.protocolVersion() == Version5 {
		msg.Properties = decodeProperties(r, &packetRemaining, propertiesUnsubscribe)
	}
	topics := make([]string, 0)
	for packetRemaining > 0 {
		topic := getString(r, &packetRemaining)
		if err := validateTopicFilter(topic); err != nil {
			return err
		}
		topics = append(topics, topic)
	}
	if len(topics) == 0 {
		return errors.New("mqtt: UNSUBSCRIBE requires at least one topic filter")
	}
	msg.Topics = topics

	return nil
}

// UnsubAck represents an MQTT UNSUBACK message.
type UnsubAck struct {
	Header
	MessageId   uint16
	ReasonCodes []ReasonCode
	Properties  Properties
}

func (msg *UnsubAck) Encode(w io.Writer) error {
	if msg.MessageId == 0 {
		return badPacketIdentifierError
	}
	buf := new(bytes.Buffer)
	setUint16(msg.MessageId, buf)
	if msg.Header.protocolVersion() == Version5 {
		if err := encodeProperties(buf, msg.Properties, propertiesUnsubAck); err != nil {
			return err
		}
		for _, reason := range msg.ReasonCodes {
			setUint8(uint8(reason), buf)
		}
	}
	return writeMessage(w, MsgUnsubAck, &msg.Header, buf, 0)
}

func (msg *UnsubAck) Decode(r io.Reader, hdr Header, packetRemaining int32, config DecoderConfig) (err error) {
	msg.Header = hdr
	defer func() { err = recoverError(err, recover()) }()
	msg.Header.Version = decoderVersion(config)
	msg.MessageId = getUint16(r, &packetRemaining)
	if msg.MessageId == 0 {
		return badPacketIdentifierError
	}
	if msg.Header.protocolVersion() == Version5 {
		msg.Properties = decodeProperties(r, &packetRemaining, propertiesUnsubAck)
		for packetRemaining > 0 {
			msg.ReasonCodes = append(msg.ReasonCodes, ReasonCode(getUint8(r, &packetRemaining)))
		}
	}
	if packetRemaining != 0 {
		return msgTooLongError
	}
	return nil
}

// PingReq represents an MQTT PINGREQ message.
type PingReq struct {
	Header
}

func (msg *PingReq) Encode(w io.Writer) error {
	return msg.Header.Encode(w, MsgPingReq, 0)
}

func (msg *PingReq) Decode(r io.Reader, hdr Header, packetRemaining int32, config DecoderConfig) error {
	if packetRemaining != 0 {
		return msgTooLongError
	}
	return nil
}

// PingResp represents an MQTT PINGRESP message.
type PingResp struct {
	Header
}

func (msg *PingResp) Encode(w io.Writer) error {
	return msg.Header.Encode(w, MsgPingResp, 0)
}

func (msg *PingResp) Decode(r io.Reader, hdr Header, packetRemaining int32, config DecoderConfig) error {
	if packetRemaining != 0 {
		return msgTooLongError
	}
	return nil
}

// Disconnect represents an MQTT DISCONNECT message.
type Disconnect struct {
	Header
	ReasonCode ReasonCode
	Properties Properties
}

func (msg *Disconnect) Encode(w io.Writer) error {
	if msg.Header.protocolVersion() != Version5 || (msg.ReasonCode == 0 && len(msg.Properties) == 0) {
		return msg.Header.Encode(w, MsgDisconnect, 0)
	}
	buf := new(bytes.Buffer)
	setUint8(uint8(msg.ReasonCode), buf)
	if err := encodeProperties(buf, msg.Properties, propertiesDisconnect); err != nil {
		return err
	}
	return writeMessage(w, MsgDisconnect, &msg.Header, buf, 0)
}

func (msg *Disconnect) Decode(r io.Reader, hdr Header, packetRemaining int32, config DecoderConfig) (err error) {
	msg.Header = hdr
	msg.Header.Version = decoderVersion(config)
	if msg.Header.protocolVersion() != Version5 {
		if packetRemaining != 0 {
			return msgTooLongError
		}
		return nil
	}
	if packetRemaining == 0 {
		return nil
	}
	defer func() { err = recoverError(err, recover()) }()
	msg.ReasonCode = ReasonCode(getUint8(r, &packetRemaining))
	if packetRemaining > 0 {
		msg.Properties = decodeProperties(r, &packetRemaining, propertiesDisconnect)
	}
	if packetRemaining != 0 {
		return msgTooLongError
	}
	return nil
}

// Auth represents the MQTT 5 AUTH packet used for extended authentication.
type Auth struct {
	Header
	ReasonCode ReasonCode
	Properties Properties
}

func (msg *Auth) Encode(w io.Writer) error {
	if msg.Header.protocolVersion() != Version5 {
		return errors.New("mqtt: AUTH requires MQTT 5")
	}
	if msg.ReasonCode == 0 && len(msg.Properties) == 0 {
		return msg.Header.Encode(w, MsgAuth, 0)
	}
	buf := new(bytes.Buffer)
	setUint8(uint8(msg.ReasonCode), buf)
	if err := encodeProperties(buf, msg.Properties, propertiesAuth); err != nil {
		return err
	}
	return writeMessage(w, MsgAuth, &msg.Header, buf, 0)
}

func (msg *Auth) Decode(r io.Reader, hdr Header, packetRemaining int32, config DecoderConfig) (err error) {
	msg.Header = hdr
	msg.Header.Version = decoderVersion(config)
	if msg.Header.protocolVersion() != Version5 {
		return errors.New("mqtt: AUTH requires MQTT 5")
	}
	if packetRemaining == 0 {
		return nil
	}
	defer func() { err = recoverError(err, recover()) }()
	msg.ReasonCode = ReasonCode(getUint8(r, &packetRemaining))
	if packetRemaining > 0 {
		msg.Properties = decodeProperties(r, &packetRemaining, propertiesAuth)
	}
	if packetRemaining != 0 {
		return msgTooLongError
	}
	return nil
}

func encodeAckCommon(w io.Writer, hdr *Header, messageId uint16, reason ReasonCode, props Properties, msgType MessageType) error {
	if messageId == 0 {
		return badPacketIdentifierError
	}
	buf := new(bytes.Buffer)
	setUint16(messageId, buf)
	if hdr.protocolVersion() == Version5 && (reason != 0 || len(props) != 0) {
		setUint8(uint8(reason), buf)
		if err := encodeProperties(buf, props, propertiesAck); err != nil {
			return err
		}
	}
	return writeMessage(w, msgType, hdr, buf, 0)
}

func decodeAckCommon(r io.Reader, hdr Header, packetRemaining int32, messageId *uint16, reason *ReasonCode, props *Properties, config DecoderConfig) (err error) {
	defer func() {
		err = recoverError(err, recover())
	}()

	*messageId = getUint16(r, &packetRemaining)
	if *messageId == 0 {
		return badPacketIdentifierError
	}
	if decoderVersion(config) == Version5 && packetRemaining > 0 {
		*reason = ReasonCode(getUint8(r, &packetRemaining))
		if packetRemaining > 0 {
			*props = decodeProperties(r, &packetRemaining, propertiesAck)
		}
	}

	if packetRemaining != 0 {
		return msgTooLongError
	}

	return nil
}

func (msg *Connect) IsValidVersion() bool {
	switch msg.ProtocolVersion {
	case 3: // MQTT 3.1
		return msg.ProtocolName == PROTOCOL_3_1
	case 4: // MQTT 3.1.1
		return msg.ProtocolName == PROTOCOL_3_1_1
	case 5: // MQTT 5.0
		return msg.ProtocolName == PROTOCOL_5_0
	default:
		return false
	}
}

func (msg *Connect) Validate() error {
	if !msg.IsValidVersion() {
		return fmt.Errorf("unsupported protocol name/version: %q/%d", msg.ProtocolName, msg.ProtocolVersion)
	}
	if msg.ReservedBit != 0 {
		return errors.New("connect reserved bit must be 0")
	}
	if len(msg.ClientId) == 0 && !msg.CleanSession {
		return errors.New("empty client id requires clean session")
	}
	if msg.ProtocolVersion == 3 && (len(msg.ClientId) < 1 || len(msg.ClientId) > 23) {
		return errors.New("client id invalid length")
	}
	return validateProtocolVersion(msg)
}

func validateProtocolVersion(msg *Connect) error {
	if !msg.IsValidVersion() {
		return fmt.Errorf("unsupported protocol name/version: %q/%d", msg.ProtocolName, msg.ProtocolVersion)
	}
	switch msg.ProtocolVersion {
	case 3: // MQTT 3.1
		return validate31(msg)
	case 4: // MQTT 3.1.1
		return validate311(msg)
	case 5:
		return validate5(msg)
	default:
		return fmt.Errorf("unsupported protocol version: %d", msg.ProtocolVersion)
	}
}

func validate5(msg Message) error { return validate311(msg) }

func validate31(msg Message) error {
	return nil
}

func validate311(msg Message) error {
	switch m := msg.(type) {
	case *Connect:
		// 3.1.1 要求 CONNECT 中的保留位必须为 0
		if m.ReservedBit != 0 {
			return errors.New("reserved bit must be 0 in 3.1.1")
		}
		// Will QoS 必须小于 3
		if m.WillFlag && m.WillQos > 2 {
			return errors.New("will QoS must be <= 2 in 3.1.1")
		}
		if !m.WillFlag && (m.WillRetain || m.WillQos != QosAtMostOnce) {
			return errors.New("will retain and QoS require will flag")
		}
		if m.PasswordFlag && !m.UsernameFlag {
			return errors.New("password flag requires username flag")
		}
		if len(m.ClientId) == 0 && !m.CleanSession {
			return errors.New("empty client id requires clean start/session")
		}

	case *Publish:
		// 3.1.1 禁止发布到 $share/ 主题
		if strings.HasPrefix(m.TopicName, "$share/") {
			return errors.New("cannot publish to $share/ topics in 3.1.1")
		}

	}
	return nil
}
