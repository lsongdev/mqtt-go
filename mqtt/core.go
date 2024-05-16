// Package mqtt implements MQTT clients and servers.
package mqtt

import (
	"errors"

	"github.com/song940/mqtt-go/proto"
)

// ConnectionErrors is an array of errors corresponding to the
// Connect return codes specified in the specification.
var ConnectionErrors = [6]error{
	nil, // Connection Accepted (not an error)
	errors.New("connection refused: unacceptable protocol version"),
	errors.New("connection refused: identifier rejected"),
	errors.New("connection refused: server unavailable"),
	errors.New("connection refused: bad user name or password"),
	errors.New("connection refused: not authorized"),
}

type retainFlag bool
type dupFlag bool

const (
	retainFalse retainFlag = false
	retainTrue             = true
	dupFalse    dupFlag    = false
	dupTrue                = true
)

// header is used to initialize a proto.Header when the zero value
// is not correct. The zero value of proto.Header is
// the equivalent of header(dupFalse, proto.QosAtMostOnce, retainFalse)
// and is correct for most messages.
func header(d dupFlag, q proto.QosLevel, r retainFlag) proto.Header {
	return proto.Header{
		DupFlag: bool(d), QosLevel: q, Retain: bool(r),
	}
}

type job struct {
	m proto.Message
	r receipt
}

type receipt chan struct{}

// Wait for the receipt to indicate that the job is done.
func (r receipt) wait() {
	// TODO: timeout
	<-r
}
