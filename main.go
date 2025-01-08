package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/lsongdev/mqtt-go/mqtt"
	"github.com/lsongdev/mqtt-go/proto"
)

var id = flag.String("id", "", "client id")
var runAsServer = flag.Bool("server", false, "run as server?")
var host = flag.String("host", "localhost:1883", "hostname of broker")
var websockets = flag.String("websocket", "", "websocket server")
var user = flag.String("user", "", "username")
var pass = flag.String("pass", "", "password")
var dump = flag.Bool("dump", false, "dump messages?")

func main() {

	flag.Parse()

	if *runAsServer {
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
		return
	}

	cc, err := mqtt.NewClient(*host)
	if err != nil {
		log.Fatal(err)
	}
	cc.Dump = *dump
	cc.ClientId = *id

	if err := cc.Connect(*user, *pass); err != nil {
		fmt.Fprintf(os.Stderr, "connect: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Connected with client id", cc.ClientId)
	defer cc.Disconnect()

	go func() {
		for m := range cc.Incoming {
			fmt.Print(m.TopicName, "\t")
			m.Payload.WritePayload(os.Stdout)
			fmt.Println("\tr: ", m.Header.Retain)
		}
	}()

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		scanner.Scan()
		input := scanner.Text()
		parts := strings.Split(input, " ")
		command := parts[0]

		switch command {
		case "/subscribe":
			if len(parts) < 2 {
				fmt.Println("Usage: /subscribe <topic>")
				continue
			}
			topic := parts[1]
			cc.Subscribe([]proto.TopicQos{{Topic: topic, Qos: proto.QosAtMostOnce}})
			fmt.Println("Subscribed to topic:", topic)
		case "/publish":
			if len(parts) < 3 {
				fmt.Println("Usage: /publish <topic> <message>")
				log.Println(parts)
				continue
			}
			topic := parts[1]
			message := parts[2]
			cc.Publish(&proto.Publish{
				Header:    proto.Header{},
				TopicName: topic,
				Payload:   proto.BytesPayload([]byte(message)),
			})
			fmt.Println("Published message to topic:", topic)
		case "/exit":
			cc.Disconnect()
			fmt.Println("Disconnected from broker")
			os.Exit(0)
		default:
			fmt.Println("Unknown command:", command)
		}
	}
}
