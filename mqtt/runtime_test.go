package mqtt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/lsongdev/mqtt-go/proto"
)

func receivePublish(t *testing.T, c *ClientConn) *proto.Publish {
	t.Helper()
	select {
	case m := <-c.Incoming:
		if m == nil {
			t.Fatal("client closed while waiting for publish")
		}
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for publish")
		return nil
	}
}

func TestRetainedDeleteIsDeliveredAndDispatcherContinues(t *testing.T) {
	server := NewServer()
	defer server.Close()

	sub := pipeClient(t, server, ClientOptions{ClientID: "sub", CleanStart: true})
	pub := pipeClient(t, server, ClientOptions{ClientID: "pub", CleanStart: true})
	if ack := sub.Subscribe([]proto.TopicQos{{Topic: "retained/#"}}); ack == nil {
		t.Fatal("subscribe failed")
	}

	if err := pub.Publish(&proto.Publish{Header: proto.Header{Retain: true}, TopicName: "retained/value", Payload: proto.BytesPayload("saved")}); err != nil {
		t.Fatal(err)
	}
	if got := receivePublish(t, sub); string(got.Payload.(proto.BytesPayload)) != "saved" {
		t.Fatalf("initial retained publish = %q", got.Payload)
	}

	if err := pub.Publish(&proto.Publish{Header: proto.Header{Retain: true}, TopicName: "retained/value", Payload: proto.BytesPayload(nil)}); err != nil {
		t.Fatal(err)
	}
	if got := receivePublish(t, sub); got.Payload.Size() != 0 {
		t.Fatalf("retained delete payload size = %d", got.Payload.Size())
	}

	if err := pub.Publish(&proto.Publish{TopicName: "retained/after", Payload: proto.BytesPayload("after")}); err != nil {
		t.Fatal(err)
	}
	if got := receivePublish(t, sub); string(got.Payload.(proto.BytesPayload)) != "after" {
		t.Fatalf("dispatcher stopped after retained delete: %q", got.Payload)
	}
}

func TestDollarTopicDoesNotMatchLeadingWildcard(t *testing.T) {
	server := NewServer()
	defer server.Close()

	general := pipeClient(t, server, ClientOptions{ClientID: "general", CleanStart: true})
	system := pipeClient(t, server, ClientOptions{ClientID: "system", CleanStart: true})
	pub := pipeClient(t, server, ClientOptions{ClientID: "publisher", CleanStart: true})
	general.Subscribe([]proto.TopicQos{{Topic: "#"}})
	system.Subscribe([]proto.TopicQos{{Topic: "$private/#"}})

	if err := pub.Publish(&proto.Publish{TopicName: "$private/test", Payload: proto.BytesPayload("ok")}); err != nil {
		t.Fatal(err)
	}
	if got := receivePublish(t, system); got.TopicName != "$private/test" {
		t.Fatalf("system subscriber got %q", got.TopicName)
	}
	select {
	case got := <-general.Incoming:
		t.Fatalf("leading wildcard matched $ topic: %#v", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestServerAssignsEmptyV5ClientID(t *testing.T) {
	server := NewServer()
	defer server.Close()
	serverSide, clientSide := net.Pipe()
	defer clientSide.Close()
	server.ServeConn(serverSide)

	connect := &proto.Connect{
		ProtocolName: proto.PROTOCOL_5_0, ProtocolVersion: 5,
		CleanSession: true, ClientId: "",
	}
	if err := connect.Encode(clientSide); err != nil {
		t.Fatal(err)
	}
	decode := &proto.DecodeOptions{Version: proto.Version5}
	message, err := proto.DecodeOneMessage(clientSide, decode)
	if err != nil {
		t.Fatal(err)
	}
	ack := message.(*proto.ConnAck)
	values := ack.Properties.Values(proto.PropertyAssignedClientIdentifier)
	if len(values) != 1 || values[0] == "" {
		t.Fatalf("assigned client id property = %#v", values)
	}

	if err := (&proto.PingReq{Header: proto.Header{Version: proto.Version5}}).Encode(clientSide); err != nil {
		t.Fatal(err)
	}
	if message, err = proto.DecodeOneMessage(clientSide, decode); err != nil {
		t.Fatal(err)
	} else if _, ok := message.(*proto.PingResp); !ok {
		t.Fatalf("after assigned id got %T", message)
	}
}

func TestPacketIdentifierWrapSkipsZero(t *testing.T) {
	c := &ClientConn{id: 65535, packetIDs: make(map[uint16]struct{})}
	id, err := c.nextid()
	if err != nil || id != 65535 {
		t.Fatalf("first id = %d, %v", id, err)
	}
	c.releaseid(id)
	c.packetIDs[1] = struct{}{}
	id, err = c.nextid()
	if err != nil || id != 2 {
		t.Fatalf("wrapped id = %d, %v", id, err)
	}
}

func TestServerKeepAliveClosesIdleConnection(t *testing.T) {
	server := NewServer()
	defer server.Close()
	serverSide, clientSide := net.Pipe()
	defer clientSide.Close()
	server.ServeConn(serverSide)

	connect := &proto.Connect{
		ProtocolName: proto.PROTOCOL_3_1_1, ProtocolVersion: 4,
		CleanSession: true, ClientId: "idle", KeepAliveTimer: 1,
	}
	if err := connect.Encode(clientSide); err != nil {
		t.Fatal(err)
	}
	if _, err := proto.DecodeOneMessage(clientSide, nil); err != nil {
		t.Fatal(err)
	}

	_ = clientSide.SetReadDeadline(time.Now().Add(3 * time.Second))
	var one [1]byte
	_, err := clientSide.Read(one[:])
	if err == nil {
		t.Fatal("idle connection remained open")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("broker did not enforce keepalive: %v", err)
	}
	if err != io.EOF {
		// net.Pipe can surface a closed-pipe error depending on which side
		// observes the close first; either way it must not be a timeout.
		t.Logf("idle connection closed with %v", err)
	}
}

func TestBrokerPacketIdentifierWrapSkipsZero(t *testing.T) {
	c := &incomingConn{
		nextID:    65535,
		packetIDs: make(map[uint16]struct{}),
		closed:    make(chan struct{}),
	}
	id := c.nextMessageID()
	if id != 65535 {
		t.Fatalf("first id = %d", id)
	}
	c.releaseMessageID(id)
	c.packetIDs[1] = struct{}{}
	id = c.nextMessageID()
	if id != 2 {
		t.Fatalf("wrapped id = %d", id)
	}
}

func TestClientKeepAliveClosesWhenPingResponseIsMissing(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	defer serverSide.Close()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		if _, err := proto.DecodeOneMessage(serverSide, nil); err != nil {
			return
		}
		if err := (&proto.ConnAck{}).Encode(serverSide); err != nil {
			return
		}
		_, _ = proto.DecodeOneMessage(serverSide, nil) // PINGREQ; intentionally no PINGRESP.
	}()

	client := NewClientConn(clientSide)
	if err := client.ConnectWithOptions(ClientOptions{
		ClientID:   "keepalive-client",
		CleanStart: true,
		KeepAlive:  1,
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-client.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("client kept connection open without PINGRESP")
	}
	<-serverDone
}

func TestClientAcceptsAssignedV5ClientID(t *testing.T) {
	server := NewServer()
	defer server.Close()

	serverSide, clientSide := net.Pipe()
	server.ServeConn(serverSide)
	client := NewClientConn(clientSide)
	defer client.Close()

	if err := client.ConnectWithOptions(ClientOptions{
		ProtocolVersion: proto.Version5,
		CleanStart:      true,
	}); err != nil {
		t.Fatal(err)
	}
	if client.ClientId == "" {
		t.Fatal("client did not retain broker-assigned client id")
	}
}

func TestServerCloseClosesPreConnectTransport(t *testing.T) {
	server := NewServer()
	serverSide, clientSide := net.Pipe()
	defer clientSide.Close()
	server.ServeConn(serverSide)

	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	_ = clientSide.SetReadDeadline(time.Now().Add(time.Second))
	var one [1]byte
	_, err := clientSide.Read(one[:])
	if err == nil {
		t.Fatal("pre-CONNECT transport remained open after Server.Close")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("Server.Close did not close pre-CONNECT transport: %v", err)
	}
}

func TestServeConnAfterServerCloseClosesTransport(t *testing.T) {
	server := NewServer()
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}

	serverSide, clientSide := net.Pipe()
	defer clientSide.Close()
	server.ServeConn(serverSide)

	_ = clientSide.SetReadDeadline(time.Now().Add(time.Second))
	var one [1]byte
	if _, err := clientSide.Read(one[:]); err == nil {
		t.Fatal("ServeConn accepted a transport after Server.Close")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("transport was left open after Server.Close: %v", err)
	}
}

func TestBrokerAuthentication(t *testing.T) {
	for _, version := range []proto.ProtocolVersion{proto.Version311, proto.Version5} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			server, err := NewServerWithOptions(ServerOptions{
				Authenticator: AuthenticateFunc(func(ctx context.Context, req AuthRequest) error {
					if req.ClientID != "auth-client" || !req.UsernamePresent || req.Username != "user" {
						return ErrNotAuthorized
					}
					if !req.PasswordPresent || string(req.Password) != "secret" {
						return ErrBadCredentials
					}
					return nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()

			ok := pipeClient(t, server, ClientOptions{
				ProtocolVersion: version,
				ClientID:        "auth-client",
				CleanStart:      true,
				Username:        "user",
				Password:        "secret",
			})
			if ok == nil {
				t.Fatal("authenticated client missing")
			}

			serverSide, clientSide := net.Pipe()
			server.ServeConn(serverSide)
			bad := NewClientConn(clientSide)
			defer bad.Close()
			err = bad.ConnectWithOptions(ClientOptions{
				ProtocolVersion: version,
				ClientID:        "auth-client-2",
				CleanStart:      true,
				Username:        "user",
				Password:        "wrong",
			})
			if !errors.Is(err, ErrNotAuthorized) && !errors.Is(err, ErrBadCredentials) {
				t.Fatalf("unexpected auth error: %v", err)
			}
		})
	}
}

func TestLastWillPublishedOnUnexpectedDisconnect(t *testing.T) {
	server := NewServer()
	defer server.Close()

	sub := pipeClient(t, server, ClientOptions{ClientID: "will-sub", CleanStart: true})
	if ack := sub.Subscribe([]proto.TopicQos{{Topic: "will/#"}}); ack == nil {
		t.Fatal("subscribe failed")
	}
	pub := pipeClient(t, server, ClientOptions{
		ClientID:   "will-pub",
		CleanStart: true,
		Will: &Will{
			Topic:   "will/device",
			Payload: []byte("offline"),
			QoS:     proto.QosAtLeastOnce,
		},
	})
	if err := pub.Close(); err != nil {
		t.Fatal(err)
	}
	got := receivePublish(t, sub)
	if got.TopicName != "will/device" || string(got.Payload.(proto.BytesPayload)) != "offline" {
		t.Fatalf("will: %#v", got)
	}
}

func TestNormalDisconnectSuppressesLastWill(t *testing.T) {
	server := NewServer()
	defer server.Close()

	sub := pipeClient(t, server, ClientOptions{ClientID: "will-sub", CleanStart: true})
	sub.Subscribe([]proto.TopicQos{{Topic: "will/#"}})
	pub := pipeClient(t, server, ClientOptions{
		ClientID:   "will-pub",
		CleanStart: true,
		Will:       &Will{Topic: "will/device", Payload: []byte("offline")},
	})
	pub.Disconnect()
	select {
	case got := <-sub.Incoming:
		t.Fatalf("normal DISCONNECT published will: %#v", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestDelayedWillCancelledBySessionResume(t *testing.T) {
	server, err := NewServerWithOptions(ServerOptions{EnablePersistentSessions: true})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	sub := pipeClient(t, server, ClientOptions{ProtocolVersion: proto.Version5, ClientID: "will-sub", CleanStart: true})
	sub.Subscribe([]proto.TopicQos{{Topic: "will/#"}})
	first := pipeClient(t, server, ClientOptions{
		ProtocolVersion: proto.Version5,
		ClientID:        "will-pub",
		CleanStart:      false,
		SessionExpiry:   5 * time.Second,
		Will: &Will{
			Topic:   "will/device",
			Payload: []byte("offline"),
			Properties: proto.Properties{}.
				Add(proto.PropertyWillDelayInterval, uint32(1)),
		},
	})
	_ = first.Close()

	waitUntil(t, func() bool {
		server.willsMu.Lock()
		defer server.willsMu.Unlock()
		return server.wills["will-pub"] != nil
	})
	resumed := pipeClient(t, server, ClientOptions{
		ProtocolVersion: proto.Version5,
		ClientID:        "will-pub",
		CleanStart:      false,
		SessionExpiry:   5 * time.Second,
	})
	if !resumed.SessionPresent {
		t.Fatal("session was not resumed")
	}
	select {
	case got := <-sub.Incoming:
		t.Fatalf("resumed session published delayed will: %#v", got)
	case <-time.After(1200 * time.Millisecond):
	}
}

func TestCleanStartPublishesPendingWillImmediately(t *testing.T) {
	server, err := NewServerWithOptions(ServerOptions{EnablePersistentSessions: true})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	sub := pipeClient(t, server, ClientOptions{ProtocolVersion: proto.Version5, ClientID: "will-sub", CleanStart: true})
	sub.Subscribe([]proto.TopicQos{{Topic: "will/#"}})
	first := pipeClient(t, server, ClientOptions{
		ProtocolVersion: proto.Version5,
		ClientID:        "will-pub",
		CleanStart:      false,
		SessionExpiry:   10 * time.Second,
		Will: &Will{
			Topic:   "will/device",
			Payload: []byte("offline"),
			Properties: proto.Properties{}.
				Add(proto.PropertyWillDelayInterval, uint32(5)),
		},
	})
	_ = first.Close()
	waitUntil(t, func() bool {
		server.willsMu.Lock()
		defer server.willsMu.Unlock()
		return server.wills["will-pub"] != nil
	})

	replacement := pipeClient(t, server, ClientOptions{
		ProtocolVersion: proto.Version5,
		ClientID:        "will-pub",
		CleanStart:      true,
	})
	_ = replacement
	got := receivePublish(t, sub)
	if string(got.Payload.(proto.BytesPayload)) != "offline" {
		t.Fatalf("will payload: %q", got.Payload)
	}
}
