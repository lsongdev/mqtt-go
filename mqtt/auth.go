package mqtt

import (
	"context"
	"errors"
	"net"

	"github.com/lsongdev/mqtt-go/proto"
)

var (
	// ErrBadCredentials rejects CONNECT as bad user name or password.
	ErrBadCredentials = errors.New("mqtt: bad user name or password")
	// ErrNotAuthorized rejects CONNECT because the client is not authorized.
	ErrNotAuthorized = errors.New("mqtt: not authorized")
)

// AuthRequest is the immutable CONNECT identity presented to an Authenticator.
type AuthRequest struct {
	ClientID        string
	Username        string
	Password        []byte
	UsernamePresent bool
	PasswordPresent bool
	ProtocolVersion proto.ProtocolVersion
	RemoteAddr      net.Addr
	Properties      proto.Properties
}

// Authenticator validates a CONNECT request before it can take over a ClientID
// or attach to a broker session.
type Authenticator interface {
	Authenticate(context.Context, AuthRequest) error
}

// AuthenticateFunc adapts a function to Authenticator.
type AuthenticateFunc func(context.Context, AuthRequest) error

func (f AuthenticateFunc) Authenticate(ctx context.Context, req AuthRequest) error {
	return f(ctx, req)
}
