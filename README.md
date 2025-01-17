# mqtt-go

MQTT Clients and Servers in Go

## example

client example

```go
package main

func main() {
  client := mqtt.NewClient("broker.hivemq.com", 1883)
  client.Connect("username", "password")
  defer client.Disconnect()
  go func() {
    for m := range cc.Incoming {
      fmt.Print(m.TopicName, m.Payload)
    }
  }()
  client.Subscribe("topic", 0)
  client.Publish("topic", 0, []byte("hello"))
  time.Sleep(1 * time.Second)
  client.Unsubscribe("topic")
}
```

server example

```go
server := mqtt.NewServer()
log.Println("Listening on", *host)
if *websockets != "" {
  log.Println("Listening on", *websockets)
  go http.ListenAndServe(*websockets, server)
}
err := mqtt.ListenAndServe(*host, server)
if err != nil {
  log.Fatal(err)
}
```

Run as a service with `/etc/systemd/system/mqtt-server.service`

```ini
[Unit]
Description=MQTT Server
Documentation=https://github.com/lsongdev/mqtt-go
After=network-online.target
Wants=network-online.target systemd-networkd-wait-online.service

[Service]
ExecStart=/usr/local/bin/mqtt -server -host 0.0.0.0:1883

[Install]
WantedBy=multi-user.target
```

## license

see [LICENSE](LICENSE)
