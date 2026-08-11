package mqtt

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/lsongdev/mqtt-go/proto"
)

func pipeClient(t *testing.T, server *Server, options ClientOptions) *ClientConn {
	t.Helper()
	serverSide, clientSide := netPipe()
	server.ServeConn(serverSide)
	client := NewClientConn(clientSide)
	if err := client.ConnectWithOptions(options); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

// Kept behind a helper so tests consistently create both ends together.
func netPipe() (net.Conn, net.Conn) { return net.Pipe() }

func waitUntil(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not satisfied")
}

func TestOptionalQoS2EndToEnd(t *testing.T) {
	for _, version := range []proto.ProtocolVersion{proto.Version311, proto.Version5} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			server, err := NewServerWithOptions(ServerOptions{EnableQoS2: true})
			if err != nil {
				t.Fatal(err)
			}
			client := pipeClient(t, server, ClientOptions{ProtocolVersion: version, ClientID: "qos2", CleanStart: true, EnableQoS2: true})
			ack := client.Subscribe([]proto.TopicQos{{Topic: "qos2/#", Qos: proto.QosExactlyOnce}})
			if version == proto.Version5 {
				if len(ack.ReasonCodes) != 1 || ack.ReasonCodes[0] != proto.ReasonCode(proto.QosExactlyOnce) {
					t.Fatalf("SUBACK: %#v", ack)
				}
			} else if len(ack.TopicsQos) != 1 || ack.TopicsQos[0] != proto.QosExactlyOnce {
				t.Fatalf("SUBACK: %#v", ack)
			}
			client.Publish(&proto.Publish{Header: proto.Header{QosLevel: proto.QosExactlyOnce}, TopicName: "qos2/test", Payload: proto.BytesPayload("once")})
			select {
			case message := <-client.Incoming:
				if message.QosLevel != proto.QosExactlyOnce || string(message.Payload.(proto.BytesPayload)) != "once" {
					t.Fatalf("message: %#v", message)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("QoS 2 timeout")
			}
			select {
			case duplicate := <-client.Incoming:
				t.Fatalf("duplicate delivery: %#v", duplicate)
			case <-time.After(30 * time.Millisecond):
			}
		})
	}
}

func TestQoS2DuplicatePacketsDeliverOnce(t *testing.T) {
	for _, version := range []proto.ProtocolVersion{proto.Version311, proto.Version5} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			server, _ := NewServerWithOptions(ServerOptions{EnableQoS2: true})
			subscriber := pipeClient(t, server, ClientOptions{ProtocolVersion: version, ClientID: "duplicate-sub", CleanStart: true, EnableQoS2: true})
			subscriber.Subscribe([]proto.TopicQos{{Topic: "duplicate", Qos: proto.QosExactlyOnce}})
			serverSide, raw := net.Pipe()
			defer raw.Close()
			server.ServeConn(serverSide)
			connect := &proto.Connect{ProtocolName: proto.PROTOCOL_3_1_1, ProtocolVersion: uint8(version), CleanSession: true, ClientId: "raw-publisher"}
			if err := connect.Encode(raw); err != nil {
				t.Fatal(err)
			}
			decode := &proto.DecodeOptions{Version: version}
			if _, err := proto.DecodeOneMessage(raw, decode); err != nil {
				t.Fatal(err)
			}
			publish := &proto.Publish{Header: proto.Header{Version: version, QosLevel: proto.QosExactlyOnce}, TopicName: "duplicate", MessageId: 42, Payload: proto.BytesPayload("once")}
			for i := 0; i < 2; i++ {
				publish.DupFlag = i > 0
				if err := publish.Encode(raw); err != nil {
					t.Fatal(err)
				}
				if ack, err := proto.DecodeOneMessage(raw, decode); err != nil {
					t.Fatal(err)
				} else if ack.(*proto.PubRec).MessageId != 42 {
					t.Fatalf("PUBREC: %#v", ack)
				}
			}
			for i := 0; i < 2; i++ {
				rel := &proto.PubRel{Header: proto.Header{Version: version}, MessageId: 42}
				if err := rel.Encode(raw); err != nil {
					t.Fatal(err)
				}
				if ack, err := proto.DecodeOneMessage(raw, decode); err != nil {
					t.Fatal(err)
				} else if ack.(*proto.PubComp).MessageId != 42 {
					t.Fatalf("PUBCOMP: %#v", ack)
				}
			}
			select {
			case message := <-subscriber.Incoming:
				if !bytes.Equal([]byte(message.Payload.(proto.BytesPayload)), []byte("once")) {
					t.Fatalf("message: %#v", message)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("delivery timeout")
			}
			select {
			case duplicate := <-subscriber.Incoming:
				t.Fatalf("duplicate: %#v", duplicate)
			case <-time.After(30 * time.Millisecond):
			}
		})
	}
}

func TestSharedSubscriptionsRoundRobin(t *testing.T) {
	for _, version := range []proto.ProtocolVersion{proto.Version311, proto.Version5} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			server, err := NewServerWithOptions(ServerOptions{EnableSharedSubscriptions: true})
			if err != nil {
				t.Fatal(err)
			}
			opts := func(id string) ClientOptions {
				return ClientOptions{ProtocolVersion: version, ClientID: id, CleanStart: true}
			}
			a := pipeClient(t, server, opts("worker-a"))
			b := pipeClient(t, server, opts("worker-b"))
			publisher := pipeClient(t, server, opts("publisher"))
			filter := []proto.TopicQos{{Topic: "$share/workers/jobs/+", Qos: proto.QosAtMostOnce}}
			if ack := a.Subscribe(filter); ack == nil {
				t.Fatal("a SUBACK missing")
			}
			if ack := b.Subscribe(filter); ack == nil {
				t.Fatal("b SUBACK missing")
			}
			for i := 0; i < 6; i++ {
				publisher.Publish(&proto.Publish{TopicName: "jobs/new", Payload: proto.BytesPayload{byte(i)}})
			}
			counts := map[*ClientConn]int{a: 0, b: 0}
			for received := 0; received < 6; received++ {
				select {
				case <-a.Incoming:
					counts[a]++
				case <-b.Incoming:
					counts[b]++
				case <-time.After(2 * time.Second):
					t.Fatal("shared delivery timeout")
				}
			}
			if counts[a] != 3 || counts[b] != 3 {
				t.Fatalf("distribution: a=%d b=%d", counts[a], counts[b])
			}
		})
	}
}

func TestQoS2AndPersistenceDisabledByDefault(t *testing.T) {
	server := NewServer()
	client := pipeClient(t, server, ClientOptions{ProtocolVersion: proto.Version5, ClientID: "defaults", CleanStart: false, SessionExpiry: time.Hour, EnableQoS2: true})
	ack := client.Subscribe([]proto.TopicQos{{Topic: "qos/#", Qos: proto.QosExactlyOnce}})
	if len(ack.ReasonCodes) != 1 || ack.ReasonCodes[0] != 1 {
		t.Fatalf("QoS 2 should be capped: %#v", ack)
	}
	client.Close()
	waitUntil(t, func() bool {
		server.sessionsMu.Lock()
		defer server.sessionsMu.Unlock()
		return server.sessions["defaults"] == nil
	})
	resumed := pipeClient(t, server, ClientOptions{ProtocolVersion: proto.Version5, ClientID: "defaults", CleanStart: false, SessionExpiry: time.Hour})
	if resumed.SessionPresent {
		t.Fatal("persistence was enabled by default")
	}
}

func TestSharedSubscriptionsDisabledByDefault(t *testing.T) {
	client := pipeClient(t, NewServer(), ClientOptions{ProtocolVersion: proto.Version5, ClientID: "no-shared", CleanStart: true})
	ack := client.Subscribe([]proto.TopicQos{{Topic: "$share/g/a", Qos: 0}})
	if len(ack.ReasonCodes) != 1 || ack.ReasonCodes[0] != 0x9e {
		t.Fatalf("SUBACK: %#v", ack)
	}
}

func TestPersistentSessionV4(t *testing.T) {
	server, err := NewServerWithOptions(ServerOptions{EnablePersistentSessions: true})
	if err != nil {
		t.Fatal(err)
	}
	first := pipeClient(t, server, ClientOptions{ProtocolVersion: proto.Version311, ClientID: "v4-session", CleanStart: false})
	first.Subscribe([]proto.TopicQos{{Topic: "offline/+", Qos: proto.QosAtLeastOnce}})
	first.Close()
	waitUntil(t, func() bool {
		server.sessionsMu.Lock()
		defer server.sessionsMu.Unlock()
		state := server.sessions["v4-session"]
		return state != nil && state.conn == nil
	})
	publisher := pipeClient(t, server, ClientOptions{ProtocolVersion: proto.Version311, ClientID: "v4-publisher", CleanStart: true})
	publisher.Publish(&proto.Publish{Header: proto.Header{QosLevel: proto.QosAtLeastOnce}, TopicName: "offline/one", Payload: proto.BytesPayload("queued")})
	waitUntil(t, func() bool {
		server.sessionsMu.Lock()
		defer server.sessionsMu.Unlock()
		return len(server.sessions["v4-session"].Queue) == 1
	})
	second := pipeClient(t, server, ClientOptions{ProtocolVersion: proto.Version311, ClientID: "v4-session", CleanStart: false})
	if !second.SessionPresent {
		t.Fatal("session present was false")
	}
	select {
	case m := <-second.Incoming:
		if string(m.Payload.(proto.BytesPayload)) != "queued" {
			t.Fatalf("message: %#v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("offline message timeout")
	}
}

func rawV5Client(t *testing.T, server *Server, id string, clean bool) (net.Conn, *proto.DecodeOptions, *proto.ConnAck) {
	t.Helper()
	serverSide, clientSide := net.Pipe()
	server.ServeConn(serverSide)
	connect := &proto.Connect{ProtocolName: proto.PROTOCOL_5_0, ProtocolVersion: 5, CleanSession: clean, ClientId: id, Properties: proto.Properties{}.Add(proto.PropertySessionExpiryInterval, uint32(3600))}
	if err := connect.Encode(clientSide); err != nil {
		t.Fatal(err)
	}
	decode := &proto.DecodeOptions{Version: proto.Version5}
	message, err := proto.DecodeOneMessage(clientSide, decode)
	if err != nil {
		t.Fatal(err)
	}
	return clientSide, decode, message.(*proto.ConnAck)
}

func TestPersistentQoS2ResumesAtPubRel(t *testing.T) {
	server, err := NewServerWithOptions(ServerOptions{EnableQoS2: true, EnablePersistentSessions: true})
	if err != nil {
		t.Fatal(err)
	}
	raw, decode, ack := rawV5Client(t, server, "qos2-session", true)
	if ack.SessionPresent {
		t.Fatal("new session reported present")
	}
	subscribe := &proto.Subscribe{Header: proto.Header{Version: proto.Version5}, MessageId: 1, Topics: []proto.TopicQos{{Topic: "stage", Qos: proto.QosExactlyOnce}}}
	if err := subscribe.Encode(raw); err != nil {
		t.Fatal(err)
	}
	if _, err := proto.DecodeOneMessage(raw, decode); err != nil {
		t.Fatal(err)
	}
	publisher := pipeClient(t, server, ClientOptions{ProtocolVersion: proto.Version5, ClientID: "stage-publisher", CleanStart: true, EnableQoS2: true})
	publisher.Publish(&proto.Publish{Header: proto.Header{QosLevel: proto.QosExactlyOnce}, TopicName: "stage", Payload: proto.BytesPayload("resume")})
	message, err := proto.DecodeOneMessage(raw, decode)
	if err != nil {
		t.Fatal(err)
	}
	publish := message.(*proto.Publish)
	if err := (&proto.PubRec{Header: proto.Header{Version: proto.Version5}, MessageId: publish.MessageId}).Encode(raw); err != nil {
		t.Fatal(err)
	}
	message, err = proto.DecodeOneMessage(raw, decode)
	if err != nil {
		t.Fatal(err)
	}
	rel := message.(*proto.PubRel)
	raw.Close()
	waitUntil(t, func() bool {
		server.sessionsMu.Lock()
		defer server.sessionsMu.Unlock()
		state := server.sessions["qos2-session"]
		return state != nil && state.conn == nil && len(state.Queue) == 1 && state.Queue[0].Stage == 1
	})
	resumed, resumeDecode, resumeAck := rawV5Client(t, server, "qos2-session", false)
	defer resumed.Close()
	if !resumeAck.SessionPresent {
		t.Fatal("session was not resumed")
	}
	message, err = proto.DecodeOneMessage(resumed, resumeDecode)
	if err != nil {
		t.Fatal(err)
	}
	resumedRel := message.(*proto.PubRel)
	if resumedRel.MessageId != rel.MessageId {
		t.Fatalf("packet id changed: %d -> %d", rel.MessageId, resumedRel.MessageId)
	}
	if err := (&proto.PubComp{Header: proto.Header{Version: proto.Version5}, MessageId: resumedRel.MessageId}).Encode(resumed); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		server.sessionsMu.Lock()
		defer server.sessionsMu.Unlock()
		return len(server.sessions["qos2-session"].Queue) == 0
	})
}

func TestSQLiteSessionSurvivesBrokerRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := OpenSQLiteSessionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServerWithOptions(ServerOptions{EnablePersistentSessions: true, SessionStore: store})
	if err != nil {
		t.Fatal(err)
	}
	options := ClientOptions{ProtocolVersion: proto.Version5, ClientID: "sqlite-session", CleanStart: true, SessionExpiry: time.Hour}
	first := pipeClient(t, server, options)
	first.Subscribe([]proto.TopicQos{{Topic: "persist/#", Qos: proto.QosAtLeastOnce}})
	first.Close()
	waitUntil(t, func() bool {
		sessions, e := store.List(context.Background())
		return e == nil && len(sessions) == 1 && len(sessions[0].Subscriptions) == 1
	})
	publisher := pipeClient(t, server, ClientOptions{ProtocolVersion: proto.Version5, ClientID: "sqlite-publisher", CleanStart: true})
	publisher.Publish(&proto.Publish{Header: proto.Header{QosLevel: proto.QosAtLeastOnce}, TopicName: "persist/one", Payload: proto.BytesPayload("durable"), Properties: proto.Properties{}.Add(proto.PropertyContentType, "text/plain")})
	waitUntil(t, func() bool {
		sessions, e := store.List(context.Background())
		return e == nil && len(sessions) == 1 && len(sessions[0].Queue) == 1
	})
	publisher.Close()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLiteSessionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restarted, err := NewServerWithOptions(ServerOptions{EnablePersistentSessions: true, SessionStore: reopened})
	if err != nil {
		t.Fatal(err)
	}
	options.CleanStart = false
	resumed := pipeClient(t, restarted, options)
	if !resumed.SessionPresent {
		t.Fatal("restored session not reported")
	}
	select {
	case m := <-resumed.Incoming:
		if string(m.Payload.(proto.BytesPayload)) != "durable" {
			t.Fatalf("message: %#v", m)
		}
		if values := m.Properties.Values(proto.PropertyContentType); len(values) != 1 || values[0] != "text/plain" {
			t.Fatalf("properties: %#v", m.Properties)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restored queue timeout")
	}
	waitUntil(t, func() bool {
		sessions, e := reopened.List(context.Background())
		return e == nil && len(sessions) == 1 && len(sessions[0].Queue) == 0
	})
	resumed.Close()
	waitUntil(t, func() bool {
		restarted.sessionsMu.Lock()
		defer restarted.sessionsMu.Unlock()
		state := restarted.sessions["sqlite-session"]
		return state != nil && state.conn == nil
	})
}

func TestSQLiteSessionExpiry(t *testing.T) {
	store, err := OpenSQLiteSessionStore(filepath.Join(t.TempDir(), "expiry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Save(context.Background(), StoredSession{ClientID: "expired", ExpiresAt: time.Now().Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("expired sessions: %#v", sessions)
	}
}
