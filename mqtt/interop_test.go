//go:build interop

package mqtt

import (
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lsongdev/mqtt-go/proto"
)

// These tests intentionally require independent implementations. Missing tools
// are failures, so CI cannot silently skip the interoperability gate.
func TestInteropPaho(t *testing.T) {
	python := os.Getenv("INTEROP_PYTHON")
	if python == "" {
		python = "python3"
	}
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("features=%t", full), func(t *testing.T) {
			server, err := NewServerWithOptions(ServerOptions{
				EnableQoS2: full, EnablePersistentSessions: full, EnableSharedSubscriptions: full,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { server.Close() })
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { listener.Close() })
			go server.Serve(listener)
			ws := httptest.NewServer(server)
			t.Cleanup(ws.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, python, "testdata/interop_paho.py",
				listener.Addr().String(), strings.TrimPrefix(ws.URL, "http://"), fmt.Sprint(full))
			output, err := cmd.CombinedOutput()
			t.Logf("Paho results:\n%s", output)
			if err != nil {
				t.Fatalf("Paho interoperability: %v", err)
			}
		})
	}
}

func startMosquitto(t *testing.T, extra string) string {
	t.Helper()
	binary := os.Getenv("INTEROP_MOSQUITTO")
	if binary == "" {
		binary = "mosquitto"
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	dir := t.TempDir()
	config := filepath.Join(dir, "mosquitto.conf")
	if err := os.WriteFile(config, []byte(fmt.Sprintf("listener %d 127.0.0.1\nallow_anonymous true\npersistence false\n%s", port, extra)), 0600); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(dir, "mosquitto.log"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, binary, "-c", config, "-v")
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		cancel()
		log.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		cmd.Wait()
		log.Close()
		if t.Failed() {
			output, _ := os.ReadFile(log.Name())
			t.Logf("Mosquitto log:\n%s", output)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return address
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Mosquitto did not start")
	return ""
}

func TestInteropMosquitto(t *testing.T) {
	address := startMosquitto(t, "")
	for _, version := range []proto.ProtocolVersion{proto.Version311, proto.Version5} {
		for _, qos := range []proto.QosLevel{0, 1, 2} {
			t.Run(fmt.Sprintf("v%d/qos%d", version, qos), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				client, err := Dial(ctx, address, ClientOptions{
					ProtocolVersion: version, CleanStart: true, EnableQoS2: true,
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { client.Close() })
				topic := fmt.Sprintf("interop/v%d/qos%d", version, qos)
				ack := client.Subscribe([]proto.TopicQos{{Topic: topic, Qos: qos}})
				if ack == nil {
					t.Fatal("SUBACK missing")
				}
				if version == proto.Version5 {
					if len(ack.ReasonCodes) != 1 || ack.ReasonCodes[0] != proto.ReasonCode(qos) {
						t.Fatalf("SUBACK: %#v", ack)
					}
				} else if len(ack.TopicsQos) != 1 || ack.TopicsQos[0] != qos {
					t.Fatalf("SUBACK: %#v", ack)
				}
				message := &proto.Publish{Header: proto.Header{QosLevel: qos}, TopicName: topic, Payload: proto.BytesPayload{0, 255, 1}}
				if version == proto.Version5 {
					message.Properties = proto.Properties{}.
						Add(proto.PropertyContentType, "application/octet-stream").
						Add(proto.PropertyUserProperty, proto.StringPair{Key: "trace", Value: "one"}).
						Add(proto.PropertyUserProperty, proto.StringPair{Key: "trace", Value: "two"})
				}
				if err := client.Publish(message); err != nil {
					t.Fatal(err)
				}
				got := receivePublish(t, client)
				if got.QosLevel != qos || string(got.Payload.(proto.BytesPayload)) != string([]byte{0, 255, 1}) {
					t.Fatalf("PUBLISH: %#v", got)
				}
				if version == proto.Version5 && len(got.Properties.Values(proto.PropertyUserProperty)) != 2 {
					t.Fatalf("lost repeated properties: %#v", got.Properties)
				}
				if ack := client.Unsubscribe([]string{topic}); ack == nil {
					t.Fatal("UNSUBACK missing")
				}
				client.Disconnect()
			})
		}
	}
}

func TestInteropMosquittoOversizedQoS2(t *testing.T) {
	address := startMosquitto(t, "message_size_limit 16\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Dial(ctx, address, ClientOptions{ProtocolVersion: proto.Version5, ClientID: "rejected", CleanStart: true, EnableQoS2: true})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	message := &proto.Publish{Header: proto.Header{QosLevel: 2}, TopicName: "interop/rejected", Payload: proto.BytesPayload(strings.Repeat("x", 32))}
	if err := client.Publish(message); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		client.idMu.Lock()
		_, used := client.packetIDs[message.MessageId]
		client.idMu.Unlock()
		if !used {
			client.qosMu.Lock()
			_, pending := client.outgoingQoS2[message.MessageId]
			client.qosMu.Unlock()
			if pending {
				t.Fatal("completed QoS 2 publish retained inflight state")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("oversized QoS 2 exchange did not release packet identifier")
}

func TestInteropMosquittoPasswordOnlyV5(t *testing.T) {
	address := startMosquitto(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Dial(ctx, address, ClientOptions{ProtocolVersion: proto.Version5, Password: "token", CleanStart: false})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if client.ClientId == "" {
		t.Fatal("assigned Client Identifier missing")
	}
}

func TestInteropMosquittoServerKeepAlive(t *testing.T) {
	// Ten seconds also works with older Mosquitto 2.0 packages in CI.
	address := startMosquitto(t, "max_keepalive 10\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Dial(ctx, address, ClientOptions{ProtocolVersion: proto.Version5, CleanStart: true, KeepAlive: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if client.keepAlive != 10*time.Second {
		t.Fatalf("server Keep Alive was not applied: %v", client.keepAlive)
	}
	select {
	case <-client.closed:
		t.Fatal("client failed to keep connection alive with negotiated interval")
	case <-time.After(16 * time.Second):
	}
}

func TestInteropMosquittoMaximumQoS(t *testing.T) {
	address := startMosquitto(t, "max_qos 1\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Dial(ctx, address, ClientOptions{ProtocolVersion: proto.Version5, CleanStart: true, EnableQoS2: true})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Publish(&proto.Publish{Header: proto.Header{QosLevel: 2}, TopicName: "interop/qos", Payload: proto.BytesPayload("too high")}); err == nil {
		t.Fatal("PUBLISH exceeded the broker's negotiated Maximum QoS")
	}
	if err := client.Publish(&proto.Publish{Header: proto.Header{QosLevel: 1}, TopicName: "interop/qos", Payload: proto.BytesPayload("allowed")}); err != nil {
		t.Fatal(err)
	}
}
