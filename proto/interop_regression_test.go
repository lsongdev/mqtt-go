package proto

import (
	"bytes"
	"testing"
)

func TestConnectVersionSpecificCredentialsAndIdentifier(t *testing.T) {
	for _, version := range []ProtocolVersion{Version311, Version5} {
		for _, passwordOnly := range []bool{false, true} {
			connect := &Connect{ProtocolName: "MQTT", ProtocolVersion: uint8(version), ClientId: "", CleanSession: false}
			if passwordOnly {
				connect.ClientId = "token-client"
				connect.CleanSession = true
				connect.PasswordFlag = true
				connect.Password = string([]byte{0xff, 0, 1})
			}
			var wire bytes.Buffer
			err := connect.Encode(&wire)
			if version == Version311 {
				if err == nil || connect.Validate() == nil {
					t.Fatalf("v4 accepted invalid CONNECT: %#v", connect)
				}
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			message, err := DecodeOneMessage(&wire, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := message.(*Connect)
			if err := got.Validate(); err != nil {
				t.Fatal(err)
			}
			if got.ClientId != connect.ClientId || got.CleanSession != connect.CleanSession || got.UsernameFlag || got.Password != connect.Password {
				t.Fatalf("CONNECT changed: %#v", got)
			}
		}
	}
}
