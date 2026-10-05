"""Independent Paho clients exercise mqtt-go over loopback TCP and WebSocket."""

import queue
import socket
import struct
import sys
import time
import unittest
import uuid

import paho.mqtt.client as mqtt
from paho.mqtt.packettypes import PacketTypes
from paho.mqtt.properties import Properties
from paho.mqtt.subscribeoptions import SubscribeOptions


TCP, WS, FULL = sys.argv[1:4]
sys.argv[1:] = []
FULL = FULL == "true"


class WirePeer:
    """Small wire probe for boundary cases Paho's public API cannot express."""
    def __init__(self, flags=2, client_id=b"", password=None):
        host, port = TCP.split(":")
        self.conn = socket.create_connection((host, int(port)), timeout=3)
        body = b"\x00\x04MQTT\x05" + bytes([flags]) + b"\x00\x00\x00"
        body += struct.pack("!H", len(client_id)) + client_id
        if password is not None:
            body += struct.pack("!H", len(password)) + password
        self.conn.sendall(bytes([0x10, len(body)]) + body)

    def packet(self):
        header = self.conn.recv(1)
        if not header:
            raise EOFError("connection closed before packet")
        remaining, shift = 0, 0
        while True:
            encoded = self.conn.recv(1)
            if not encoded:
                raise EOFError("truncated packet length")
            byte = encoded[0]
            remaining |= (byte & 127) << shift
            if not byte & 128:
                break
            shift += 7
            if shift >= 28:
                raise ValueError("invalid packet length")
        body = b""
        while len(body) < remaining:
            chunk = self.conn.recv(remaining - len(body))
            if not chunk:
                raise EOFError("truncated packet")
            body += chunk
        return header[0], body


class Peer:
    def __init__(self, version=5, transport="tcp", client_id=None, expiry=0,
                 clean=True, will=None, connect_properties=None, manual_ack=False):
        self.messages = queue.Queue()
        self.connections = queue.Queue()
        self.subacks = queue.Queue()
        self.unsubacks = queue.Queue()
        self.disconnects = queue.Queue()
        kwargs = {} if version == 5 else {"clean_session": clean}
        self.client = mqtt.Client(
            mqtt.CallbackAPIVersion.VERSION2,
            client_id=client_id or uuid.uuid4().hex,
            protocol=mqtt.MQTTv5 if version == 5 else mqtt.MQTTv311,
            transport=transport, reconnect_on_failure=False, manual_ack=manual_ack, **kwargs)
        self.client.on_connect = lambda c, u, f, r, p: self.connections.put((f, r, p))
        self.client.on_message = lambda c, u, m: self.messages.put(m)
        self.client.on_subscribe = lambda c, u, mid, reasons, p: self.subacks.put(reasons)
        self.client.on_unsubscribe = lambda c, u, mid, reasons, p: self.unsubacks.put(reasons)
        self.client.on_disconnect = lambda c, u, f, r, p: self.disconnects.put(r)
        if will:
            self.client.will_set(*will)
        host, port = (WS if transport == "websockets" else TCP).split(":")
        if transport == "websockets":
            self.client.ws_set_options(path="/mqtt")
        if version == 5:
            props = connect_properties or Properties(PacketTypes.CONNECT)
            if expiry:
                props.SessionExpiryInterval = expiry
            self.client.connect(host, int(port), keepalive=2, clean_start=clean, properties=props)
        else:
            self.client.connect(host, int(port), keepalive=2)
        self.client.loop_start()
        self.connack = self.connections.get(timeout=3)
        if self.connack[1].is_failure:
            self.close()
            raise RuntimeError(f"CONNACK rejected: {self.connack[1].value:#x}")

    def close(self):
        self.client.disconnect()
        self.client.loop_stop()

    def subscribe(self, topic, qos=0, options=None, properties=None):
        if options is None:
            self.client.subscribe(topic, qos, properties=properties)
        else:
            self.client.subscribe(topic, options=options, properties=properties)
        return self.subacks.get(timeout=3)

    def publish(self, topic, payload, qos=0, retain=False, properties=None):
        info = self.client.publish(topic, payload, qos, retain, properties)
        info.wait_for_publish(timeout=3)
        assert info.is_published(), "publish acknowledgement timed out"


class Interoperability(unittest.TestCase):
    def peer(self, **kwargs):
        peer = Peer(**kwargs)
        self.addCleanup(peer.close)
        return peer

    def test_qos_binary_and_properties(self):
        for version in (4, 5):
            for transport in ("tcp", "websockets"):
                for qos in range(3 if FULL else 2):
                    with self.subTest(version=version, transport=transport, qos=qos):
                        sub = self.peer(version=version, transport=transport)
                        pub = self.peer(version=version, transport=transport)
                        topic = "interop/" + uuid.uuid4().hex
                        reasons = sub.subscribe(topic, qos)
                        self.assertEqual(reasons[0].value, qos)
                        props = None
                        if version == 5:
                            props = Properties(PacketTypes.PUBLISH)
                            props.ContentType = "application/octet-stream"
                            props.UserProperty = [("trace", "one"), ("trace", "two")]
                            props.CorrelationData = b"\x00\xff"
                        pub.publish(topic, b"\x00\xff\x01", qos, properties=props)
                        message = sub.messages.get(timeout=3)
                        self.assertEqual((message.topic, message.payload, message.qos),
                                         (topic, b"\x00\xff\x01", qos))
                        if version == 5:
                            self.assertEqual(message.properties.UserProperty, props.UserProperty)
                            self.assertEqual(message.properties.CorrelationData, b"\x00\xff")
                        sub.close()
                        pub.close()

    def test_retained_and_delete(self):
        for version in (4, 5):
            with self.subTest(version=version):
                topic = "interop/" + uuid.uuid4().hex
                pub = self.peer(version=version)
                pub.publish(topic, b"saved", 1, retain=True)
                sub = self.peer(version=version)
                granted = sub.subscribe(topic, 2)[0].value
                message = sub.messages.get(timeout=3)
                self.assertTrue(message.retain)
                self.assertEqual(message.payload, b"saved")
                self.assertLessEqual(message.qos, granted)
                pub.publish(topic, b"", 1, retain=True)
                self.assertEqual(sub.messages.get(timeout=3).payload, b"")
                late = self.peer(version=version)
                late.subscribe(topic)
                with self.assertRaises(queue.Empty):
                    late.messages.get(timeout=0.15)

    def test_unsubscribe_reasons(self):
        peer = self.peer()
        topic = "interop/" + uuid.uuid4().hex
        peer.subscribe(topic)
        peer.client.unsubscribe([topic, topic + "/absent"])
        self.assertEqual([r.value for r in peer.unsubacks.get(timeout=3)], [0, 0x11])

    def test_no_local_and_retain_handling(self):
        peer = self.peer()
        topic = "interop/" + uuid.uuid4().hex
        peer.publish(topic, b"saved", 1, retain=True)
        peer.subscribe(topic, options=SubscribeOptions(qos=1, noLocal=True, retainHandling=2))
        peer.publish(topic, b"local", 1)
        with self.assertRaises(queue.Empty):
            peer.messages.get(timeout=0.2)
        pub = self.peer()
        pub.publish(topic, b"remote", 1)
        self.assertEqual(peer.messages.get(timeout=3).payload, b"remote")

    def test_session_resume_and_offline_queue(self):
        if not FULL:
            self.skipTest("persistent sessions disabled")
        for version in (4, 5):
            with self.subTest(version=version):
                client_id = uuid.uuid4().hex
                topic = "interop/" + uuid.uuid4().hex
                sub = self.peer(version=version, client_id=client_id, clean=False, expiry=30)
                sub.subscribe(topic, 1)
                sub.client.disconnect()
                sub.disconnects.get(timeout=3)
                sub.client.loop_stop()
                pub = self.peer(version=version)
                pub.publish(topic, b"offline", 1)
                resumed = self.peer(version=version, client_id=client_id, clean=False, expiry=30)
                self.assertTrue(resumed.connack[0].session_present)
                self.assertEqual(resumed.messages.get(timeout=3).payload, b"offline")

    def test_last_will(self):
        for version in (4, 5):
            with self.subTest(version=version):
                topic = "interop/" + uuid.uuid4().hex
                sub = self.peer(version=version)
                sub.subscribe(topic, 1)
                pub = self.peer(version=version, expiry=30 if FULL else 0,
                                will=(topic, b"offline", 1, False))
                # Abrupt transport loss; a normal DISCONNECT would suppress Will.
                pub.client.socket().shutdown(socket.SHUT_RDWR)
                pub.client.loop_stop()
                self.assertEqual(sub.messages.get(timeout=3).payload, b"offline")

    def test_shared_subscription(self):
        a, b, pub = self.peer(), self.peer(), self.peer()
        topic = "interop/" + uuid.uuid4().hex
        shared = "$share/workers/" + topic
        reasons_a, reasons_b = a.subscribe(shared, 1), b.subscribe(shared, 1)
        if not FULL:
            self.assertEqual(reasons_a[0].value, 0x9e)
            self.assertEqual(reasons_b[0].value, 0x9e)
            return
        for i in range(4):
            pub.publish(topic, str(i), 1)
        received_a = [a.messages.get(timeout=3).payload for _ in range(2)]
        received_b = [b.messages.get(timeout=3).payload for _ in range(2)]
        self.assertEqual(sorted(received_a + received_b), [b"0", b"1", b"2", b"3"])

    def test_subscription_identifiers_capability(self):
        peer = self.peer()
        available = getattr(peer.connack[2], "SubscriptionIdentifierAvailable", 1)
        props = Properties(PacketTypes.SUBSCRIBE)
        props.SubscriptionIdentifier = 17
        topic = "interop/" + uuid.uuid4().hex
        reasons = peer.subscribe(topic, 1, properties=props)
        if available == 0:
            self.assertEqual(reasons[0].value, 0xa1)
        else:
            self.assertEqual(reasons[0].value, 1)
            pub = self.peer()
            pub.publish(topic, b"identified", 1)
            message = peer.messages.get(timeout=3)
            self.assertEqual(getattr(message.properties, "SubscriptionIdentifier", []), [17])

    def test_reject_unnegotiated_topic_alias(self):
        peer = self.peer()
        self.assertEqual(getattr(peer.connack[2], "TopicAliasMaximum", 0), 0)
        # Paho 2.1.0 ignores DISCONNECT reason codes at Remaining Length <= 2.
        # Check the actual wire reason instead of its misleading callback value.
        wire = WirePeer()
        self.addCleanup(wire.conn.close)
        self.assertEqual(wire.packet()[0], 0x20)
        body = b"\x00\x0dinterop/alias\x03\x23\x00\x01invalid alias"
        wire.conn.sendall(bytes([0x30, len(body)]) + body)
        header, body = wire.packet()
        self.assertEqual((header, body[0]), (0xe0, 0x94))

    def test_session_expiry_starts_at_disconnect(self):
        if not FULL:
            self.skipTest("persistent sessions disabled")
        client_id = uuid.uuid4().hex
        topic = "interop/" + uuid.uuid4().hex
        sub = self.peer(client_id=client_id, clean=False, expiry=1)
        sub.subscribe(topic, 1)
        time.sleep(1.2)
        sub.client.disconnect()
        sub.disconnects.get(timeout=3)
        sub.client.loop_stop()
        pub = self.peer()
        pub.publish(topic, b"after long connection", 1)
        resumed = self.peer(client_id=client_id, clean=False, expiry=1)
        self.assertTrue(resumed.connack[0].session_present)
        self.assertEqual(resumed.messages.get(timeout=3).payload, b"after long connection")

    def test_enhanced_auth_is_rejected(self):
        props = Properties(PacketTypes.CONNECT)
        props.AuthenticationMethod = "unsupported-method"
        # Unsupported enhanced authentication must fail at CONNACK instead of
        # silently accepting a connection without the requested AUTH exchange.
        with self.assertRaisesRegex(RuntimeError, "0x8c"):
            self.peer(connect_properties=props)

    def test_receive_maximum_flow_control(self):
        for qos in range(1, 3 if FULL else 2):
            with self.subTest(qos=qos):
                props = Properties(PacketTypes.CONNECT)
                props.ReceiveMaximum = 1
                sub = self.peer(connect_properties=props, manual_ack=True)
                pub = self.peer()
                topic = "interop/" + uuid.uuid4().hex
                sub.subscribe(topic, qos)
                for i in range(3):
                    pub.publish(topic, str(i), qos)
                for i in range(3):
                    message = sub.messages.get(timeout=3)
                    self.assertEqual(message.payload, str(i).encode())
                    with self.assertRaises(queue.Empty):
                        sub.messages.get(timeout=0.15)
                    self.assertEqual(sub.client.ack(message.mid, message.qos), mqtt.MQTT_ERR_SUCCESS)

    def test_disabled_persistence_is_advertised(self):
        if FULL:
            self.skipTest("persistent sessions enabled")
        peer = self.peer(expiry=30)
        self.assertEqual(getattr(peer.connack[2], "SessionExpiryInterval", 30), 0)

    def test_shared_subscription_does_not_replay_retained(self):
        if not FULL:
            self.skipTest("shared subscriptions disabled")
        pub, sub = self.peer(), self.peer()
        topic = "interop/" + uuid.uuid4().hex
        pub.publish(topic, b"retained", 1, retain=True)
        self.assertEqual(sub.subscribe("$share/workers/" + topic, 1)[0].value, 1)
        with self.assertRaises(queue.Empty):
            sub.messages.get(timeout=0.15)

    def test_retained_delivery_uses_granted_qos(self):
        if FULL:
            self.skipTest("Maximum QoS is unrestricted")
        topic = "interop/" + uuid.uuid4().hex
        # A v3.1.1 Will can supply a retained QoS 2 message even when new
        # subscriptions are capped at QoS 1 by the default broker profile.
        observer = self.peer(version=4)
        observer.subscribe(topic, 1)
        pub = self.peer(version=4, will=(topic, b"qos2 will", 2, True))
        pub.client.socket().shutdown(socket.SHUT_RDWR)
        pub.client.loop_stop()
        observer.messages.get(timeout=3)
        sub = self.peer(version=4)
        self.assertEqual(sub.subscribe(topic, 2)[0].value, 1)
        message = sub.messages.get(timeout=3)
        self.assertEqual((message.qos, message.retain), (1, True))

    def test_v5_connect_boundaries(self):
        for flags, client_id, password in ((0, b"", None), (0x42, b"token-client", b"token")):
            with self.subTest(flags=flags):
                wire = WirePeer(flags, client_id, password)
                self.addCleanup(wire.conn.close)
                header, ack = wire.packet()
                self.assertEqual((header, ack[1]), (0x20, 0))
                if not client_id:
                    self.assertIn(b"\x12", ack[3:], "Assigned Client Identifier missing")
                wire.conn.sendall(b"\xe0\x00")


if __name__ == "__main__":
    unittest.main(verbosity=2)
