package mqtt

import (
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// WebSocketConn wraps a websocket connection to implement net.Conn interface
type WebSocketConn struct {
	*websocket.Conn
	reader    io.Reader
	closeOnce sync.Once
	readMu    sync.Mutex
	writeMu   sync.Mutex
}

// wsUpgrader specifies parameters for upgrading an HTTP connection to a WebSocket connection
var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:   1024,
	WriteBufferSize:  1024,
	HandshakeTimeout: 10 * time.Second,
	// 支持 MQTT WebSocket 子协议
	Subprotocols: []string{"mqttv3.1", "mqtt"},
}

// WebSocketConfig holds the configuration for WebSocket connections
type WebSocketConfig struct {
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	Path         string
	TLSConfig    *tls.Config
}

// NewWebSocketConn creates a new WebSocketConn.
func NewWebSocketConn(conn *websocket.Conn) *WebSocketConn {
	return &WebSocketConn{Conn: conn}
}

// Read exposes MQTT bytes as a continuous stream across WebSocket binary
// messages without buffering a complete frame in memory.
func (w *WebSocketConn) Read(p []byte) (int, error) {
	w.readMu.Lock()
	defer w.readMu.Unlock()
	for {
		if w.reader == nil {
			messageType, reader, err := w.NextReader()
			if err != nil {
				if _, ok := err.(*websocket.CloseError); ok {
					return 0, io.EOF
				}
				return 0, err
			}
			if messageType != websocket.BinaryMessage {
				return 0, fmt.Errorf("mqtt: websocket requires binary messages")
			}
			w.reader = reader
		}
		n, err := w.reader.Read(p)
		if err == io.EOF {
			w.reader = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

// Write implements io.Writer interface
func (w *WebSocketConn) Write(p []byte) (n int, err error) {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	writer, err := w.NextWriter(websocket.BinaryMessage)
	if err != nil {
		closeErr, ok := err.(*websocket.CloseError)
		if ok {
			log.Printf("WebSocket: Write close error for %s: %v", w.RemoteAddr(), err)
			if closeErr.Code == websocket.CloseNormalClosure ||
				closeErr.Code == websocket.CloseGoingAway {
				return 0, io.EOF
			}
			if closeErr.Code == websocket.CloseAbnormalClosure ||
				closeErr.Code == websocket.CloseNoStatusReceived {
				return 0, io.EOF
			}
		}
		if err.Error() == "use of closed network connection" {
			log.Printf("WebSocket: Connection closed when writing to %s", w.RemoteAddr())
			return 0, io.EOF
		}
		log.Printf("WebSocket: Write error for %s: %v", w.RemoteAddr(), err)
		return 0, err
	}

	n, err = writer.Write(p)
	if err != nil {
		log.Printf("WebSocket: Error writing message to %s: %v", w.RemoteAddr(), err)
		return n, err
	}
	log.Printf("WebSocket: Wrote %d bytes to %s", n, w.RemoteAddr())

	err = writer.Close()
	if err != nil {
		log.Printf("WebSocket: Error closing writer for %s: %v", w.RemoteAddr(), err)
	}
	return n, err
}

// Close implements io.Closer interface
func (w *WebSocketConn) Close() error {
	var err error
	w.closeOnce.Do(func() {
		deadline := time.Now().Add(time.Second)
		_ = w.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), deadline)
		err = w.Conn.Close()
	})
	return err
}

// SetDeadline implements net.Conn interface
func (w *WebSocketConn) SetDeadline(t time.Time) error {
	if err := w.SetReadDeadline(t); err != nil {
		return err
	}
	return w.SetWriteDeadline(t)
}

// ServeHTTP implements http.Handler for MQTT over WebSocket.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	wsConn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("mqtt: websocket upgrade: %v", err)
		return
	}
	// Bound a single WebSocket frame. MQTT packet limits are enforced by the
	// protocol decoder independently; frames can be smaller than packets.
	wsConn.SetReadLimit(16 << 20)
	s.ServeConn(NewWebSocketConn(wsConn))
}
