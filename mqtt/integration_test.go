package mqtt

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/lsongdev/mqtt-go/proto"
)

func TestBrokerV4AndV5(t *testing.T) {
	for _, version := range []proto.ProtocolVersion{proto.Version311, proto.Version5} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			serverSide, clientSide := net.Pipe()
			server := NewServer()
			server.ServeConn(serverSide)
			client := NewClientConn(clientSide)
			if err := client.ConnectWithOptions(ClientOptions{ProtocolVersion: version, ClientID: "integration", CleanStart: true}); err != nil {
				t.Fatal(err)
			}
			ack := client.Subscribe([]proto.TopicQos{{Topic: "test/+", Qos: proto.QosAtLeastOnce}})
			if version == proto.Version5 {
				if len(ack.ReasonCodes) != 1 || ack.ReasonCodes[0] != 1 {
					t.Fatalf("SUBACK reasons: %v", ack.ReasonCodes)
				}
			} else if len(ack.TopicsQos) != 1 || ack.TopicsQos[0] != proto.QosAtLeastOnce {
				t.Fatalf("SUBACK qos: %v", ack.TopicsQos)
			}
			client.Publish(&proto.Publish{Header: proto.Header{QosLevel: proto.QosAtLeastOnce}, TopicName: "test/one", Payload: proto.BytesPayload("hello")})
			select {
			case message := <-client.Incoming:
				if message.TopicName != "test/one" || message.QosLevel != proto.QosAtLeastOnce || string(message.Payload.(proto.BytesPayload)) != "hello" {
					t.Fatalf("message: %#v", message)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for publish")
			}
			if version == proto.Version5 {
				client.Publish(&proto.Publish{Header: proto.Header{Retain: true}, TopicName: "retained/one", Payload: proto.BytesPayload("saved")})
				client.Subscribe([]proto.TopicQos{{Topic: "retained/#", Qos: proto.QosAtMostOnce, RetainHandling: 2}})
				select {
				case m := <-client.Incoming:
					t.Fatalf("retain handling 2 delivered %#v", m)
				case <-time.After(30 * time.Millisecond):
				}
				if ack := client.Unsubscribe([]string{"retained/#"}); ack == nil || len(ack.ReasonCodes) != 1 {
					t.Fatalf("UNSUBACK: %#v", ack)
				}
				client.Subscribe([]proto.TopicQos{{Topic: "retained/#", Qos: proto.QosAtMostOnce}})
				select {
				case m := <-client.Incoming:
					if !m.Retain || string(m.Payload.(proto.BytesPayload)) != "saved" {
						t.Fatalf("retained message: %#v", m)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("retained message timeout")
				}
			}
			client.Disconnect()
		})
	}
}

func TestConnectContextTimeout(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	defer serverSide.Close()
	client := NewClientConn(clientSide)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	err := client.ConnectContext(ctx, ClientOptions{ProtocolVersion: proto.Version5, ClientID: "timeout", CleanStart: true})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestConnectValidatesOptionsBeforeWriting(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	defer serverSide.Close()
	client := NewClientConn(clientSide)
	defer client.Close()
	err := client.ConnectWithOptions(ClientOptions{ProtocolVersion: proto.Version311, ClientID: "invalid", CleanStart: true, Password: "secret"})
	if err == nil {
		t.Fatal("password without username was accepted")
	}
}
