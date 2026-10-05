package mqtt

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/lsongdev/mqtt-go/proto"
)

func readV5Packet(t *testing.T, conn net.Conn) proto.Message {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	message, err := proto.DecodeOneMessage(conn, &proto.DecodeOptions{Version: proto.Version5})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func writeV5Packet(t *testing.T, conn net.Conn, message proto.Message) {
	t.Helper()
	proto.SetVersion(message, proto.Version5)
	if err := conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := message.Encode(conn); err != nil {
		t.Fatal(err)
	}
}

func TestClientNegativePubRecEndsExchange(t *testing.T) {
	broker, transport := net.Pipe()
	defer broker.Close()
	client := NewClientConn(transport)
	defer client.Close()
	result := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		result <- client.ConnectContext(ctx, ClientOptions{ProtocolVersion: proto.Version5, ClientID: "negative-pubrec", CleanStart: true, EnableQoS2: true})
	}()
	if _, ok := readV5Packet(t, broker).(*proto.Connect); !ok {
		t.Fatal("CONNECT missing")
	}
	writeV5Packet(t, broker, &proto.ConnAck{})
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	go func() {
		result <- client.Publish(&proto.Publish{Header: proto.Header{QosLevel: 2}, TopicName: "rejected", Payload: proto.BytesPayload("payload")})
	}()
	publish := readV5Packet(t, broker).(*proto.Publish)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	writeV5Packet(t, broker, &proto.PubRec{MessageId: publish.MessageId, ReasonCode: 0x80})
	go func() { result <- client.sync(&proto.PingReq{}) }()
	if _, ok := readV5Packet(t, broker).(*proto.PingReq); !ok {
		t.Fatal("negative PUBREC incorrectly caused PUBREL")
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	writeV5Packet(t, broker, &proto.PingResp{})
	select {
	case <-client.pingresp:
	case <-time.After(2 * time.Second):
		t.Fatal("PINGRESP missing")
	}
	client.idMu.Lock()
	_, leased := client.packetIDs[publish.MessageId]
	client.idMu.Unlock()
	client.qosMu.Lock()
	_, pending := client.outgoingQoS2[publish.MessageId]
	client.qosMu.Unlock()
	if leased || pending {
		t.Fatal("negative PUBREC retained packet identifier or QoS 2 state")
	}
}

func TestBrokerNegativePubRecRemovesDurableDelivery(t *testing.T) {
	server, err := NewServerWithOptions(ServerOptions{EnableQoS2: true, EnablePersistentSessions: true})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	raw, _, _ := rawV5Client(t, server, "rejecting-subscriber", true)
	defer raw.Close()
	writeV5Packet(t, raw, &proto.Subscribe{MessageId: 1, Topics: []proto.TopicQos{{Topic: "negative", Qos: 2}}})
	if _, ok := readV5Packet(t, raw).(*proto.SubAck); !ok {
		t.Fatal("SUBACK missing")
	}
	pub := pipeClient(t, server, ClientOptions{ProtocolVersion: proto.Version5, ClientID: "negative-publisher", CleanStart: true, EnableQoS2: true})
	if err := pub.Publish(&proto.Publish{Header: proto.Header{QosLevel: 2}, TopicName: "negative", Payload: proto.BytesPayload("reject")}); err != nil {
		t.Fatal(err)
	}
	publish := readV5Packet(t, raw).(*proto.Publish)
	writeV5Packet(t, raw, &proto.PubRec{MessageId: publish.MessageId, ReasonCode: 0x80})
	writeV5Packet(t, raw, &proto.PingReq{})
	if _, ok := readV5Packet(t, raw).(*proto.PingResp); !ok {
		t.Fatal("negative PUBREC incorrectly caused PUBREL")
	}
	server.sessionsMu.Lock()
	queued := len(server.sessions["rejecting-subscriber"].Queue)
	server.sessionsMu.Unlock()
	if queued != 0 {
		t.Fatal("rejected delivery remained in persistent queue")
	}
}

func TestV5UnsubscribeReportsAbsentFilters(t *testing.T) {
	server := NewServer()
	defer server.Close()
	client := pipeClient(t, server, ClientOptions{ProtocolVersion: proto.Version5, ClientID: "unsubscribe", CleanStart: true})
	if ack := client.Subscribe([]proto.TopicQos{{Topic: "exact"}, {Topic: "wild/+"}}); ack == nil {
		t.Fatal("SUBACK missing")
	}
	ack := client.Unsubscribe([]string{"exact", "exact", "wild/+", "missing/#"})
	if ack == nil || len(ack.ReasonCodes) != 4 {
		t.Fatalf("UNSUBACK: %#v", ack)
	}
	for i, expected := range []proto.ReasonCode{0, 0x11, 0, 0x11} {
		if ack.ReasonCodes[i] != expected {
			t.Fatalf("UNSUBACK reasons: %v", ack.ReasonCodes)
		}
	}
}

func TestV5SessionLifetimeBeginsAtDisconnect(t *testing.T) {
	server, err := NewServerWithOptions(ServerOptions{EnablePersistentSessions: true})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client := pipeClient(t, server, ClientOptions{ProtocolVersion: proto.Version5, ClientID: "expiry-clock", CleanStart: true, SessionExpiry: time.Minute})
	if ack := client.Subscribe([]proto.TopicQos{{Topic: "expiry/#", Qos: 1}}); ack == nil {
		t.Fatal("SUBACK missing")
	}
	server.sessionsMu.Lock()
	expires := server.sessions["expiry-clock"].ExpiresAt
	server.sessionsMu.Unlock()
	if !expires.IsZero() {
		t.Fatal("connected Session has an expiry deadline")
	}
	before := time.Now()
	client.Disconnect()
	waitUntil(t, func() bool {
		server.sessionsMu.Lock()
		defer server.sessionsMu.Unlock()
		state := server.sessions["expiry-clock"]
		return state != nil && state.conn == nil
	})
	server.sessionsMu.Lock()
	expires = server.sessions["expiry-clock"].ExpiresAt
	server.sessionsMu.Unlock()
	if expires.Before(before.Add(time.Minute)) || expires.After(time.Now().Add(time.Minute)) {
		t.Fatalf("expiry deadline %v does not start at disconnect", expires)
	}
}

func TestBrokerDoesNotForwardIncomingDupFlag(t *testing.T) {
	server := NewServer()
	defer server.Close()
	sub := pipeClient(t, server, ClientOptions{ProtocolVersion: proto.Version5, ClientID: "dup-receiver", CleanStart: true})
	sub.Subscribe([]proto.TopicQos{{Topic: "dup", Qos: 0}})
	raw, _, _ := rawV5Client(t, server, "dup-sender", true)
	defer raw.Close()
	writeV5Packet(t, raw, &proto.Publish{Header: proto.Header{QosLevel: 1, DupFlag: true}, MessageId: 1, TopicName: "dup", Payload: proto.BytesPayload("retransmitted upstream")})
	if _, ok := readV5Packet(t, raw).(*proto.PubAck); !ok {
		t.Fatal("PUBACK missing")
	}
	got := receivePublish(t, sub)
	if got.DupFlag || got.QosLevel != 0 {
		t.Fatalf("forwarded PUBLISH: %#v", got)
	}
}

func TestAssignedClientIDDoesNotTakeOverNamedClient(t *testing.T) {
	server := NewServer()
	defer server.Close()
	named := pipeClient(t, server, ClientOptions{ProtocolVersion: proto.Version5, ClientID: "mqtt-1", CleanStart: true})
	assigned := pipeClient(t, server, ClientOptions{ProtocolVersion: proto.Version5, CleanStart: false})
	if assigned.ClientId == named.ClientId {
		t.Fatal("assigned Client Identifier replaced an existing Session")
	}
	if ack := named.Subscribe([]proto.TopicQos{{Topic: "still-online"}}); ack == nil {
		t.Fatal("client requesting an assigned identifier disconnected a named client")
	}
}

func TestBrokerReceiveMaximumAllowsControlPackets(t *testing.T) {
	for _, qos := range []proto.QosLevel{1, 2} {
		t.Run(string(rune('0'+qos)), func(t *testing.T) {
			server, err := NewServerWithOptions(ServerOptions{EnableQoS2: true})
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			transport, raw := net.Pipe()
			defer raw.Close()
			server.ServeConn(transport)
			writeV5Packet(t, raw, &proto.Connect{ProtocolName: "MQTT", ProtocolVersion: 5,
				ClientId: "limited", CleanSession: true,
				Properties: proto.Properties{}.Add(proto.PropertyReceiveMaximum, uint16(1))})
			if _, ok := readV5Packet(t, raw).(*proto.ConnAck); !ok {
				t.Fatal("CONNACK missing")
			}
			writeV5Packet(t, raw, &proto.Subscribe{MessageId: 1, Topics: []proto.TopicQos{{Topic: "limited", Qos: qos}}})
			if _, ok := readV5Packet(t, raw).(*proto.SubAck); !ok {
				t.Fatal("SUBACK missing")
			}
			pub := pipeClient(t, server, ClientOptions{ProtocolVersion: proto.Version5, ClientID: "flow-publisher", CleanStart: true, EnableQoS2: true})
			for _, value := range []string{"one", "two", "three"} {
				if err := pub.Publish(&proto.Publish{Header: proto.Header{QosLevel: qos}, TopicName: "limited", Payload: proto.BytesPayload(value)}); err != nil {
					t.Fatal(err)
				}
			}
			first := readV5Packet(t, raw).(*proto.Publish)
			writeV5Packet(t, raw, &proto.PingReq{})
			if _, ok := readV5Packet(t, raw).(*proto.PingResp); !ok {
				t.Fatal("PUBLISH exceeded Receive Maximum or blocked PINGRESP")
			}
			ack := func(id uint16) {
				if qos == 2 {
					writeV5Packet(t, raw, &proto.PubRec{MessageId: id, ReasonCode: 0x80})
				} else {
					writeV5Packet(t, raw, &proto.PubAck{MessageId: id})
				}
			}
			ack(first.MessageId)
			second := readV5Packet(t, raw).(*proto.Publish)
			ack(first.MessageId) // A duplicate acknowledgement cannot grant quota.
			writeV5Packet(t, raw, &proto.PingReq{})
			if _, ok := readV5Packet(t, raw).(*proto.PingResp); !ok {
				t.Fatal("duplicate acknowledgement incorrectly released quota")
			}
			ack(second.MessageId)
			third := readV5Packet(t, raw).(*proto.Publish)
			if string(first.Payload.(proto.BytesPayload)) != "one" || string(second.Payload.(proto.BytesPayload)) != "two" || string(third.Payload.(proto.BytesPayload)) != "three" {
				t.Fatal("deferred PUBLISH order changed")
			}
			ack(third.MessageId)
		})
	}
}
