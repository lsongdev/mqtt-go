# Interoperability tests

The interoperability suite runs independent MQTT implementations on loopback
interfaces. It creates temporary Mosquitto processes and ephemeral TCP/HTTP
listeners, then closes them after each test. It does not use public brokers or
start a system service. Missing tools cause a test failure, rather than a skip.

## Run locally

Install Mosquitto (`brew install mosquitto` on macOS, or
`sudo apt-get install mosquitto` on Ubuntu), then install the pinned Python
client in a virtual environment outside the checkout:

```sh
python3 -m venv /tmp/mqtt-go-interop
/tmp/mqtt-go-interop/bin/python -m pip install -r mqtt/testdata/requirements-interop.txt
INTEROP_PYTHON=/tmp/mqtt-go-interop/bin/python make interop
```

If the broker executable is not on PATH, supply its absolute path with
`INTEROP_MOSQUITTO`. For a Homebrew installation this may be
`/opt/homebrew/opt/mosquitto/sbin/mosquitto`.

The command enables the `interop` build tag and the Go race detector. Ordinary
`go test ./...` does not need Python or Mosquitto and includes the local
regression tests for the fixes. CI runs both suites in separate jobs.

## Matrix

| Direction | Versions / transports | Scenarios |
| --- | --- | --- |
| Paho Python → mqtt-go broker | MQTT 3.1.1 and 5.0; TCP and WebSocket | Binary payloads, QoS 0/1/2, repeatable User Properties and Correlation Data |
| Paho Python → mqtt-go broker | MQTT 3.1.1 and 5.0; TCP | Retained delivery/deletion, granted QoS, Last Will, persistent session resume and offline messages |
| Paho Python → mqtt-go broker | MQTT 5.0; TCP | No Local, Retain Handling, shared subscriptions, Receive Maximum for QoS 1/2, session expiry timing, capability negotiation, unsubscribe reason codes |
| Independent wire probes → mqtt-go broker | MQTT 5.0; TCP | Empty Client Identifier without Clean Start, password without username, invalid Topic Alias reason |
| mqtt-go client → Mosquitto | MQTT 3.1.1 and 5.0; TCP | QoS 0/1/2, binary payloads, subscribe/unsubscribe, ordered repeatable properties |
| mqtt-go client → Mosquitto | MQTT 5.0; TCP | Password-only CONNECT, assigned identifier, Server Keep Alive override and connection survival, Maximum QoS, oversized QoS 2 completion |

The embedded broker is exercised with both its default profile and all optional
in-memory features enabled. Tests for intentionally disabled features skip only
their corresponding profile; supported configurations still run them.

## Findings and fixes

Interoperability and regression probes cover the following corrections:

- MQTT 5 CONNECT inherited MQTT 3.1.1's restrictions on password-only
  authentication and empty identifiers without Clean Start. Validation now
  applies these restrictions only to MQTT 3.1.1, and the v5 client preserves
  the requested Clean Start value when requesting an assigned identifier.
- The broker implicitly advertised Subscription Identifier support but dropped
  identifiers. It now advertises the feature as unavailable and returns SUBACK
  reason `0xa1` for requests using it.
- The broker accepted Topic Aliases despite advertising the default maximum
  of zero. It now sends DISCONNECT reason `0x94` before closing the connection.
- Unsupported enhanced authentication was silently accepted. The broker now
  rejects the CONNECT with CONNACK reason `0x8c`.
- UNSUBACK returned success for absent filters. It now reports `0x11` per absent
  filter, including duplicate filters in one UNSUBSCRIBE.
- Session expiry counted from CONNECT instead of transport closure. A connected
  session now has no expiry deadline; disconnect starts its expiry clock. When
  persistence is disabled, CONNACK explicitly overrides the requested expiry
  to zero.
- A persistent session with no Will Delay deferred its Will until session
  expiry. Zero Will Delay now publishes immediately.
- The broker ignored Receive Maximum. Its writer now delays excess QoS 1/2
  PUBLISH packets until acknowledgements release quota, while keeping control
  packets moving and maintaining bounded queues.
- The client ignored Maximum QoS in CONNACK. Unsupported publishes now return
  an error before sending and leave the connection usable.
- Shared subscriptions replayed retained messages, and retained delivery could
  exceed the granted subscription QoS. Shared subscriptions now suppress
  retained replay; normal retained delivery uses the granted QoS.

Local regression probes also cover immediate peer closure after a successful
write, terminal negative PUBREC on both client and broker (including durable
queue removal), independent downstream DUP flags, and assigned identifiers
that do not replace an existing named client. The client also applies Server
Keep Alive from CONNACK, as verified against Mosquitto.

Paho Python 2.1.0 ignores the reason byte of a DISCONNECT with Remaining Length
of one or two. The invalid-alias test checks actual packet bytes with an
independent wire probe so that this external defect cannot mask a broker error.
No Paho workaround is included in the library itself.

## Validation and limits

Validated locally with Mosquitto 2.1.2 and Paho Python 2.1.0. The CI job installs
the Ubuntu Mosquitto package and pins Paho to the same version. The suite covers
the matrix above; it is not a claim of exhaustive MQTT conformance. TLS,
cross-process SQLite recovery with external clients, Message Expiry handling,
and all MQTT 5 negotiation limits are outside this interoperability matrix.
SQLite recovery and additional lifecycle cases remain covered by the ordinary
Go tests.

Protocol expectations are based on the
[OASIS MQTT 5.0 standard](https://docs.oasis-open.org/mqtt/mqtt/v5.0/mqtt-v5.0.html)
and the [MQTT 3.1.1 standard](https://docs.oasis-open.org/mqtt/mqtt/v3.1.1/os/mqtt-v3.1.1-os.html).
