# Architecture

mqtt-go is intentionally split into two library layers plus a small command.

## Packages

### `proto`

`proto` owns the MQTT wire format only:

- fixed headers and Remaining Length
- MQTT 3.1.1 / MQTT 5 packet encoding and decoding
- MQTT 5 property types and packet-specific property validation
- UTF-8, topic name/filter, packet identifier, and packet-size validation
- payload streaming primitives

It does not own broker routing, sessions, reconnect policy, or application callbacks.

### `mqtt`

`mqtt` owns protocol state and runtime behavior:

- client connection state, Keep Alive, packet identifier leases, QoS handshakes
- managed TCP reconnect and subscription replay above the one-transport client
- broker CONNECT authentication, connection state, Last Will, and session lifecycle
- ordered subscription routing and retained messages
- optional persistent sessions and SQLite storage
- transport adapters such as WebSocket

### `cmd/mqtt`

The command is an example/debug CLI. It is not part of the library API.

## Runtime invariants

The implementation relies on a small set of invariants:

1. A connection has exactly one writer goroutine. No other goroutine writes MQTT bytes directly.
2. Broker routing has one dispatcher. This preserves ordered-topic delivery; socket writes still happen concurrently per connection.
3. Per-client outbound queues are bounded. A full queue closes the slow connection rather than dropping MQTT control packets.
4. Connection queues are never closed by producers. Connection shutdown is signaled separately, avoiding send-on-closed-channel races.
5. Packet identifier 0 is never allocated or accepted where an identifier is required. Client identifiers remain leased until the matching acknowledgement completes.
6. Decoder size limits are checked before payload allocation.
7. `proto` rejects malformed wire state; `mqtt` handles valid packet state transitions.
8. Authentication completes before a connection can take over a ClientID or attach to Session state.
9. Last Will belongs to the connection/session lifecycle: normal DISCONNECT discards it; abnormal close schedules it; Session resume can cancel a delayed Will.
10. Automatic reconnect does not mutate `ClientConn` semantics. A `ReconnectingClient` owns successive `ClientConn` values and never silently queues application publishes while disconnected.

## Session persistence

`SessionStore` is the persistence boundary. Broker logic owns MQTT session semantics; a store only serializes and restores `StoredSession` values.

The built-in SQLite store is optional. The default broker remains fully in-memory.
Pending delayed Wills are also in-memory and are not restored after a broker
process restart.

## Testing

The CI quality gate runs:

```sh
gofmt
go vet ./...
go test ./...
go test -race ./...
```

Protocol and runtime tests include exact wire encodings, malformed packets,
MQTT 5 property validation, CONNECT authentication, Last Will/Will Delay,
automatic reconnect and subscription replay, QoS flows, persistent sessions,
retained-message behavior, topic matching, Keep Alive, packet identifier
wraparound, and a decoder fuzz target.
