# proto

Package `proto` provides streaming MQTT packet encoding and decoding for MQTT
3.1.1 (protocol level 4) and MQTT 5.0 (protocol level 5). It does not implement
broker semantics.

The zero-value packet header uses MQTT 3.1.1. For MQTT 5 packets other than
CONNECT, set `Header.Version` or call `proto.SetVersion(message, proto.Version5)`.
When decoding packets after a v5 CONNECT, pass a reusable
`&proto.DecodeOptions{Version: proto.Version5}` to `DecodeOneMessage`.

MQTT 5 properties use `Properties`, an ordered slice of typed `Property`
values. This retains duplicate properties where the specification permits
them. See the repository README for examples and the broker capability matrix.

The `mqtt` package builds optional QoS 2 state machines, persistent sessions
(including a SQLite store), and `$share/{group}/{filter}` scheduling on top of
this codec.
