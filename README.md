# mqtt-go

`mqtt-go` is a small, embeddable MQTT client, broker, and packet codec written
in Go. It supports MQTT protocol level 4 (MQTT 3.1.1) and protocol level 5
(MQTT 5.0) without requiring a separate broker process. SQLite persistence is
available when durable sessions are enabled; the default profile stays fully
in memory.

## Install

```sh
go get github.com/lsongdev/mqtt-go
```

The module requires Go 1.22 or newer.

## Client

`Dial` is the recommended API: it opens the transport and completes the MQTT
handshake in one context-aware call.

```go
package main

import (
    "context"
    "fmt"
    "log"
    "time"

    "github.com/lsongdev/mqtt-go/mqtt"
    "github.com/lsongdev/mqtt-go/proto"
)

func main() {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    client, err := mqtt.Dial(ctx, "broker.hivemq.com:1883", mqtt.ClientOptions{
        ProtocolVersion: proto.Version5, // use proto.Version311 for MQTT 3.1.1
        ClientID:        "embedded-example",
        CleanStart:      true,
        KeepAlive:       30,
        EnableQoS2:      true,      // optional
        SessionExpiry:   time.Hour, // v5 persistent session
        MaxPacketSize:   4 << 20,  // optional; runtime default is 16 MiB
    })
    if err != nil {
        log.Fatal(err)
    }
    defer client.Disconnect()

    ack := client.Subscribe([]proto.TopicQos{{
        Topic: "example/+",
        Qos:   proto.QosAtMostOnce,
    }})
    fmt.Printf("subscribed: %#v\n", ack)

    if err := client.Publish(&proto.Publish{
        TopicName: "example/hello",
        Payload:   proto.BytesPayload("hello"),
    }); err != nil {
        log.Fatal(err)
    }
}
```

For a custom transport (TLS, an in-memory pipe, or another `net.Conn`), use
`mqtt.NewClientConn(conn)` followed by `ConnectWithOptions`. The older
`NewClient` and `Connect(user, pass)` APIs remain available and default to
MQTT 3.1.1.

For a long-running TCP client, `DialWithReconnect` adds redial with bounded
exponential backoff while keeping a stable `Incoming` channel. Successful
subscriptions are replayed only when the broker reports that no previous
Session is present.

```go
client, err := mqtt.DialWithReconnect(ctx, address, mqtt.ClientOptions{
    ProtocolVersion: proto.Version5,
    ClientID:        "worker-1",
    CleanStart:      false,
    SessionExpiry:   time.Hour,
}, mqtt.ReconnectOptions{
    MinDelay: 250 * time.Millisecond,
    MaxDelay: 30 * time.Second,
})
```

Application publishes are not silently queued while disconnected:
`Publish` returns `mqtt.ErrClientClosed`, leaving retry/idempotency policy to
the application.

## Embedded broker

The broker accepts a caller-owned listener, so lifecycle, TLS, and port
selection remain under application control.

```go
listener, err := net.Listen("tcp", "127.0.0.1:1883")
if err != nil {
    log.Fatal(err)
}
defer listener.Close()

broker := mqtt.NewServer()
defer broker.Close()
if err := broker.Serve(listener); err != nil && !errors.Is(err, net.ErrClosed) {
    log.Fatal(err)
}
```

Use `broker.ServeConn(conn)` to embed MQTT over a caller-managed transport.
`mqtt.ListenAndServe(address, broker)` is the convenience TCP API. `Server`
also implements `http.Handler` for MQTT over WebSocket.

Optional features are enabled explicitly:

```go
store, err := mqtt.OpenSQLiteSessionStore("mqtt-sessions.db")
if err != nil {
    log.Fatal(err)
}
defer store.Close()

broker, err := mqtt.NewServerWithOptions(mqtt.ServerOptions{
    EnableQoS2:                true,
    EnablePersistentSessions:  true,
    EnableSharedSubscriptions: true,
    SessionStore:              store,
    MaxPacketSize:             8 << 20,
    Authenticator: mqtt.AuthenticateFunc(func(ctx context.Context, req mqtt.AuthRequest) error {
        if req.Username != "device" || string(req.Password) != "secret" {
            return mqtt.ErrBadCredentials
        }
        return nil
    }),
})
if err != nil {
    log.Fatal(err)
}
defer broker.Close()
```

Providing `SessionStore` also enables persistent sessions. Leave it nil for
in-memory sessions. Client and broker runtimes reject packets larger than
`mqtt.DefaultMaxPacketSize` (16 MiB) by default, before payload allocation.
Set `MaxPacketSize` explicitly when the application requires a different bound.

Client Last Will is configured directly in `ClientOptions`. MQTT 5 Will
properties, including Will Delay, are preserved when the Will becomes a
PUBLISH message.

```go
Will: &mqtt.Will{
    Topic:   "devices/worker-1/status",
    Payload: []byte("offline"),
    QoS:     proto.QosAtLeastOnce,
    Retain:  true,
    Properties: proto.Properties{}.
        Add(proto.PropertyWillDelayInterval, uint32(10)),
},
```

## Protocol support

| Capability | MQTT 3.1.1 (v4) | MQTT 5.0 (v5) |
| --- | --- | --- |
| CONNECT / CONNACK | yes | yes, including properties |
| PUBLISH | QoS 0/1; optional QoS 2 | QoS 0/1; optional QoS 2 and properties |
| SUBSCRIBE / UNSUBSCRIBE | yes | yes, including options/properties/reason codes |
| Retained messages and `+` / `#` filters | in memory | in memory |
| PING / DISCONNECT | yes | yes, including reason code/properties |
| PUBACK/PUBREC/PUBREL/PUBCOMP codec | yes | yes |
| CONNECT authentication callback | yes | yes |
| AUTH packet codec | n/a | yes |
| Last Will | yes | yes, including Will Delay |
| Broker/client QoS 2 flow | optional, four-step handshake | optional, four-step handshake |
| Persistent sessions / offline queue | optional, memory or SQLite | optional, expiry and SQLite recovery |
| Shared subscriptions | optional `$share/{group}/{filter}` | optional `$share/{group}/{filter}` |

The packet codec is in `proto`. MQTT 5 properties are represented by an
ordered `proto.Properties` slice, preserving repeatable properties such as
User Property and Subscription Identifier. The codec validates fixed-header
flags, packet identifiers, UTF-8 and topic syntax, canonical Variable Byte
Integers, property value types, duplicate rules, and packet-specific MQTT 5
property contexts while decoding and encoding.

```go
props := proto.Properties{}.
    Add(proto.PropertyContentType, "application/json").
    Add(proto.PropertyUserProperty, proto.StringPair{Key: "trace", Value: "abc"})

message := &proto.Publish{
    Header:     proto.Header{Version: proto.Version5},
    TopicName:  "events",
    Properties: props,
    Payload:    proto.BytesPayload(`{"ok":true}`),
}
```

## Optional feature behavior

- QoS 2 is enabled with `EnableQoS2` on both the broker and client. Incoming
  messages are released only after PUBREL, duplicate PUBLISH/PUBREL packets do
  not cause duplicate application delivery, and broker delivery completes on
  PUBCOMP.
- Persistent sessions are enabled with `EnablePersistentSessions` or by
  supplying a `SessionStore`. MQTT 3.1.1 uses `CleanStart: false`; MQTT 5 uses
  `CleanStart: false` when resuming and `SessionExpiry` to set lifetime.
- The SQLite store persists subscriptions, MQTT 5 properties, expiry, and
  offline QoS 1/2 messages. It uses the pure-Go `modernc.org/sqlite` driver and
  does not require CGO. Retained-message durability is outside the session
  store and remains in memory.
- Shared subscriptions use `$share/{group}/{filter}` and select one connected
  group member per message using round-robin scheduling. When disabled, MQTT 5
  returns reason code `0x9e` and the broker advertises the feature as unavailable.

For persistent broker-to-client QoS 2 delivery, the Packet ID and PUBLISH or
PUBREL stage are durable and resume after reconnect.

- `Authenticator` runs before a CONNECT can take over a ClientID or attach to
  a Session. `ErrBadCredentials` and `ErrNotAuthorized` map to the
  corresponding MQTT 3.1.1 and MQTT 5 CONNACK reason codes.
- Last Will is discarded only by a normal DISCONNECT. MQTT 5 delayed Will is
  published when the Will Delay expires or the Session ends, whichever happens
  first, and is cancelled when the same Session is resumed in time.
- `DialWithReconnect` is deliberately layered above `ClientConn`: raw
  custom transports remain one-connection primitives, while the managed TCP
  client owns redial and subscription replay.

## Architecture

The implementation keeps wire semantics separate from runtime state:

- `proto/` owns MQTT packet encoding, decoding, and protocol validation.
- `mqtt/` owns client/broker state machines, ordered routing, sessions, QoS,
  persistence, and transports.
- `cmd/mqtt/` is only the example/debug executable.

See [docs/architecture.md](docs/architecture.md) for the runtime invariants.

## Testing

```sh
gofmt -w .
go vet ./...
go test ./...
go test -race ./...
go test -fuzz=FuzzDecodeOneMessage ./proto
```

CI enforces formatting, vet, unit/integration tests, and the race detector.
An additional interoperability job uses independent Eclipse Paho Python
clients against the embedded broker over TCP and WebSocket, and the Go client
against a local Eclipse Mosquitto broker. Run it with `make interop` after
installing the tools described in [docs/interoperability.md](docs/interoperability.md).
The suite covers exact v4/v5 wire encodings, malformed and truncated packets,
MQTT 5 property contexts and value constraints, topic/filter syntax, packet
identifier wraparound, packet-size limits, retained-message semantics,
Keep Alive, CONNECT authentication, Last Will and Will Delay, automatic
reconnect/subscription replay, optional QoS 2, shared subscriptions, session
resume, SQLite restart recovery, expiry, and offline queue acknowledgement.
The protocol decoder also has a fuzz target that asserts arbitrary input does
not panic.

## Scope

The broker now includes CONNECT authentication and Last Will delivery, and the
managed TCP client supports automatic reconnect. MQTT 5 enhanced
challenge/response authentication (AUTH exchange), per-topic authorization,
and persistence of pending delayed Wills across a broker process restart are
not implemented. These boundaries remain explicit rather than presenting
partial semantics as complete support.

The broker advertises Subscription Identifiers as unavailable and rejects
subscriptions requesting them. Topic Alias Maximum is zero; incoming aliases
are rejected with MQTT 5 reason `0x94`. Enhanced authentication requests are
rejected at CONNACK with reason `0x8c`. When persistent sessions are disabled,
the broker overrides a requested MQTT 5 Session Expiry Interval to zero.

## License

See the package license files in `mqtt/` and `proto/`.
