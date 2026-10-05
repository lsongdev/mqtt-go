package proto

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

func encodePacket(t *testing.T, m Message) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := m.Encode(&b); err != nil {
		t.Fatalf("encode %T: %v", m, err)
	}
	return b.Bytes()
}

func TestConnectV5WireFormatAndRoundTrip(t *testing.T) {
	m := &Connect{ProtocolName: PROTOCOL_5_0, ProtocolVersion: 5, CleanSession: true,
		KeepAliveTimer: 60, ClientId: "cid",
		Properties: Properties{}.Add(PropertySessionExpiryInterval, uint32(10))}
	want := []byte{0x10, 0x15, 0, 4, 'M', 'Q', 'T', 'T', 5, 2, 0, 60, 5, 0x11, 0, 0, 0, 10, 0, 3, 'c', 'i', 'd'}
	got := encodePacket(t, m)
	if !bytes.Equal(got, want) {
		t.Fatalf("wire packet\n got %x\nwant %x", got, want)
	}
	decoded, err := DecodeOneMessage(bytes.NewReader(got), nil)
	if err != nil {
		t.Fatal(err)
	}
	c := decoded.(*Connect)
	if c.ProtocolVersion != 5 || c.ClientId != "cid" || !reflect.DeepEqual(c.Properties, m.Properties) {
		t.Fatalf("round trip: %#v", c)
	}
}

func TestPublishWireFormats(t *testing.T) {
	v4 := &Publish{TopicName: "a", Payload: BytesPayload("hi")}
	if got, want := encodePacket(t, v4), []byte{0x30, 5, 0, 1, 'a', 'h', 'i'}; !bytes.Equal(got, want) {
		t.Fatalf("v4 got %x want %x", got, want)
	}
	v5 := &Publish{Header: Header{Version: Version5}, TopicName: "a", Payload: BytesPayload("hi"),
		Properties: Properties{}.Add(PropertyUser, StringPair{"k", "v"})}
	wire := encodePacket(t, v5)
	want := []byte{0x30, 13, 0, 1, 'a', 7, 0x26, 0, 1, 'k', 0, 1, 'v', 'h', 'i'}
	if !bytes.Equal(wire, want) {
		t.Fatalf("v5 got %x want %x", wire, want)
	}
	decoded, err := DecodeOneMessage(bytes.NewReader(wire), &DecodeOptions{Version: Version5})
	if err != nil {
		t.Fatal(err)
	}
	p := decoded.(*Publish)
	if string(p.Payload.(BytesPayload)) != "hi" || !reflect.DeepEqual(p.Properties, v5.Properties) {
		t.Fatalf("round trip: %#v", p)
	}
}

func TestV5ControlPacketsRoundTrip(t *testing.T) {
	packets := []Message{
		&ConnAck{Header: Header{Version: Version5}, Properties: Properties{}.Add(PropertyMaximumQoS, byte(1))},
		&PubAck{Header: Header{Version: Version5}, MessageId: 7, ReasonCode: 0x10, Properties: Properties{}.Add(PropertyReasonString, "stored")},
		&Subscribe{Header: Header{Version: Version5}, MessageId: 8, Topics: []TopicQos{{Topic: "a/+", Qos: QosAtLeastOnce, NoLocal: true}}},
		&SubAck{Header: Header{Version: Version5}, MessageId: 8, ReasonCodes: []ReasonCode{1}},
		&Unsubscribe{Header: Header{Version: Version5}, MessageId: 9, Topics: []string{"a/+"}},
		&UnsubAck{Header: Header{Version: Version5}, MessageId: 9, ReasonCodes: []ReasonCode{0}},
		&Disconnect{Header: Header{Version: Version5}, ReasonCode: 4, Properties: Properties{}.Add(PropertyServerReference, "other")},
		&Auth{Header: Header{Version: Version5}, ReasonCode: 0x18, Properties: Properties{}.Add(PropertyAuthenticationMethod, "token")},
	}
	for _, original := range packets {
		t.Run(reflect.TypeOf(original).Elem().Name(), func(t *testing.T) {
			wire := encodePacket(t, original)
			decoded, err := DecodeOneMessage(bytes.NewReader(wire), &DecodeOptions{Version: Version5})
			if err != nil {
				t.Fatalf("decode %x: %v", wire, err)
			}
			if reflect.TypeOf(decoded) != reflect.TypeOf(original) {
				t.Fatalf("got %T", decoded)
			}
		})
	}
}

func TestV4ControlPacketsRoundTrip(t *testing.T) {
	packets := []Message{
		&Connect{ProtocolName: PROTOCOL_3_1_1, ProtocolVersion: 4, CleanSession: true, ClientId: "client"},
		&ConnAck{SessionPresent: true},
		&Publish{Header: Header{QosLevel: QosAtLeastOnce}, TopicName: "a", MessageId: 1, Payload: BytesPayload("x")},
		&PubAck{MessageId: 1}, &PubRec{MessageId: 1}, &PubRel{MessageId: 1}, &PubComp{MessageId: 1},
		&Subscribe{MessageId: 2, Topics: []TopicQos{{Topic: "a/#", Qos: QosAtLeastOnce}}},
		&SubAck{MessageId: 2, TopicsQos: []QosLevel{QosAtLeastOnce}},
		&Unsubscribe{MessageId: 3, Topics: []string{"a/#"}}, &UnsubAck{MessageId: 3},
		&PingReq{}, &PingResp{}, &Disconnect{},
	}
	for _, original := range packets {
		t.Run(reflect.TypeOf(original).Elem().Name(), func(t *testing.T) {
			wire := encodePacket(t, original)
			if _, err := DecodeOneMessage(bytes.NewReader(wire), nil); err != nil {
				t.Fatalf("decode %x: %v", wire, err)
			}
		})
	}
}

func TestAllPropertyWireTypesRoundTrip(t *testing.T) {
	props := Properties{}.
		Add(PropertyPayloadFormatIndicator, byte(1)).
		Add(PropertyReceiveMaximum, uint16(10)).
		Add(PropertySessionExpiryInterval, uint32(20)).
		Add(PropertySubscriptionIdentifier, VarInt(321)).
		Add(PropertyCorrelationData, []byte{1, 2, 3}).
		Add(PropertyContentType, "text/plain").
		Add(PropertyUser, StringPair{Key: "a", Value: "b"})
	m := &Auth{Header: Header{Version: Version5}, ReasonCode: 0x18, Properties: props}
	decoded, err := DecodeOneMessage(bytes.NewReader(encodePacket(t, m)), &DecodeOptions{Version: Version5})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.(*Auth).Properties, props) {
		t.Fatalf("properties: %#v", decoded)
	}
}

func TestTruncatedPacketReturnsError(t *testing.T) {
	inputs := [][]byte{{0x10}, {0x10, 0x80, 0x80, 0x80, 0x80}, {0x30, 3, 0, 2, 'a'}}
	for _, input := range inputs {
		if _, err := DecodeOneMessage(bytes.NewReader(input), nil); err == nil {
			t.Fatalf("accepted %x", input)
		}
	}
}

func TestRejectsMalformedFixedHeader(t *testing.T) {
	// SUBSCRIBE must have flags 0b0010.
	if _, err := DecodeOneMessage(bytes.NewReader([]byte{0x80, 0}), nil); err == nil {
		t.Fatal("accepted malformed SUBSCRIBE")
	}
	// A QoS 0 PUBLISH cannot have DUP set.
	if _, err := DecodeOneMessage(bytes.NewReader([]byte{0x38, 0}), nil); err == nil {
		t.Fatal("accepted malformed PUBLISH")
	}
}

func TestPropertyTypeIsChecked(t *testing.T) {
	m := &Disconnect{Header: Header{Version: Version5}, Properties: Properties{}.Add(PropertySessionExpiryInterval, "wrong")}
	if err := m.Encode(new(bytes.Buffer)); err == nil {
		t.Fatal("accepted wrong property value type")
	}
}


func TestRejectsZeroPacketIdentifiers(t *testing.T) {
	cases := []Message{
		&Publish{Header: Header{QosLevel: QosAtLeastOnce}, TopicName: "a", MessageId: 0, Payload: BytesPayload("x")},
		&PubAck{MessageId: 0},
		&Subscribe{MessageId: 0, Topics: []TopicQos{{Topic: "a"}}},
		&Unsubscribe{MessageId: 0, Topics: []string{"a"}},
	}
	for _, packet := range cases {
		if err := packet.Encode(new(bytes.Buffer)); err == nil {
			t.Fatalf("encoded %T with packet identifier 0", packet)
		}
	}

	// QoS 1 PUBLISH with Packet Identifier 0.
	if _, err := DecodeOneMessage(bytes.NewReader([]byte{0x32, 5, 0, 1, 'a', 0, 0}), nil); err == nil {
		t.Fatal("decoded PUBLISH with packet identifier 0")
	}
}

func TestSubscribeAndUnsubscribeRequirePayload(t *testing.T) {
	if err := (&Subscribe{MessageId: 1}).Encode(new(bytes.Buffer)); err == nil {
		t.Fatal("encoded empty SUBSCRIBE")
	}
	if err := (&Unsubscribe{MessageId: 1}).Encode(new(bytes.Buffer)); err == nil {
		t.Fatal("encoded empty UNSUBSCRIBE")
	}
	if _, err := DecodeOneMessage(bytes.NewReader([]byte{0x82, 2, 0, 1}), nil); err == nil {
		t.Fatal("decoded empty SUBSCRIBE")
	}
	if _, err := DecodeOneMessage(bytes.NewReader([]byte{0xA2, 2, 0, 1}), nil); err == nil {
		t.Fatal("decoded empty UNSUBSCRIBE")
	}
}

func TestDecodePacketSizeLimitBeforeAllocation(t *testing.T) {
	_, err := DecodeOneMessage(bytes.NewReader([]byte{0x30, 9}), &DecodeOptions{MaxPacketSize: 8})
	if !errors.Is(err, ErrPacketTooLarge) {
		t.Fatalf("got %v", err)
	}
}

func FuzzDecodeOneMessage(f *testing.F) {
	seeds := [][]byte{
		{0xC0, 0x00},
		{0xD0, 0x00},
		{0x30, 0x03, 0x00, 0x01, 'a'},
		{0x10, 0x0C, 0x00, 0x04, 'M', 'Q', 'T', 'T', 0x04, 0x02, 0x00, 0x00, 0x00, 0x00},
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("decoder panicked for %x: %v", data, recovered)
			}
		}()
		_, _ = DecodeOneMessage(bytes.NewReader(data), &DecodeOptions{MaxPacketSize: 1 << 20})
	})
}
