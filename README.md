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

    client.Publish(&proto.Publish{
        TopicName: "example/hello",
        Payload:   proto.BytesPayload("hello"),
    })
}
```

For a custom transport (TLS, an in-memory pipe, or another `net.Conn`), use
`mqtt.NewClientConn(conn)` followed by `ConnectWithOptions`. The older
`NewClient` and `Connect(user, pass)` APIs remain available and default to
MQTT 3.1.1.

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
})
if err != nil {
    log.Fatal(err)
}
defer broker.Close()
```

Providing `SessionStore` also enables persistent sessions. Leave it nil for
in-memory sessions, or omit all options for the original lightweight profile.

## Protocol support

| Capability | MQTT 3.1.1 (v4) | MQTT 5.0 (v5) |
| --- | --- | --- |
| CONNECT / CONNACK | yes | yes, including properties |
| PUBLISH | QoS 0/1; optional QoS 2 | QoS 0/1; optional QoS 2 and properties |
| SUBSCRIBE / UNSUBSCRIBE | yes | yes, including options/properties/reason codes |
| Retained messages and `+` / `#` filters | in memory | in memory |
| PING / DISCONNECT | yes | yes, including reason code/properties |
| PUBACK/PUBREC/PUBREL/PUBCOMP codec | yes | yes |
| AUTH codec | n/a | yes |
| Broker/client QoS 2 flow | optional, four-step handshake | optional, four-step handshake |
| Persistent sessions / offline queue | optional, memory or SQLite | optional, expiry and SQLite recovery |
| Shared subscriptions | optional `$share/{group}/{filter}` | optional `$share/{group}/{filter}` |

The packet codec is in `proto`. MQTT 5 properties are represented by an
ordered `proto.Properties` slice, preserving repeatable properties such as
User Property and Subscription Identifier. Values are type checked during
encoding.

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
PUBREL stage are durable and resume after reconnect. Client-to-broker handshake
state is connection-local; after a transport loss the client retransmits its
PUBLISH/PUBREL as required by MQTT.

## Testing

```sh
go test ./...
go test -race ./...
```

The suite covers exact v4/v5 wire encodings, MQTT 5 properties and control
packets, malformed fixed headers, optional QoS 2 for v4/v5, shared-subscription
round robin, v4 session resume, SQLite restart recovery, expiry, and offline
queue acknowledgement.

## License

See the package license files in `mqtt/` and `proto/`.
