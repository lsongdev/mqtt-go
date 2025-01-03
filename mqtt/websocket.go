package mqtt

import (
	"bytes"
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
	reader      io.Reader
	closeChan   chan struct{}
	closeOnce   sync.Once
	readTimeout time.Duration
	readBuf     bytes.Buffer // 添加缓冲区用于合并消息
	mu          sync.Mutex   // 保护并发访问
}

// wsUpgrader specifies parameters for upgrading an HTTP connection to a WebSocket connection
var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:   1024,
	WriteBufferSize:  1024,
	HandshakeTimeout: 10 * time.Second,
	// 允许所有源的请求,生产环境应该配置具体的检查逻辑
	CheckOrigin: func(r *http.Request) bool { return true },
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

// NewWebSocketConn creates a new WebSocketConn
func NewWebSocketConn(conn *websocket.Conn) *WebSocketConn {
	wsConn := &WebSocketConn{
		Conn:        conn,
		closeChan:   make(chan struct{}),
		readTimeout: 60 * time.Second, // 默认60秒超时
	}

	// 设置WebSocket连接参数
	conn.SetPongHandler(func(string) error {
		log.Printf("WebSocket: Received pong from %s", conn.RemoteAddr())
		return conn.SetReadDeadline(time.Now().Add(wsConn.readTimeout))
	})

	conn.SetPingHandler(func(data string) error {
		log.Printf("WebSocket: Received ping from %s", conn.RemoteAddr())
		err := conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(10*time.Second))
		if err != nil {
			log.Printf("WebSocket: Failed to send pong to %s: %v", conn.RemoteAddr(), err)
		}
		return conn.SetReadDeadline(time.Now().Add(wsConn.readTimeout))
	})

	// 启动心跳协程
	go wsConn.startHeartbeat()

	log.Printf("WebSocket: New connection from %s", conn.RemoteAddr())
	return wsConn
}

// Read implements io.Reader interface
func (w *WebSocketConn) Read(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// 如果缓冲区中有数据，先从缓冲区读取
	if w.readBuf.Len() > 0 {
		return w.readBuf.Read(p)
	}

	// 清空缓冲区
	w.readBuf.Reset()

	// 读取新的WebSocket消息
	for {
		if w.reader == nil {
			messageType, reader, err := w.NextReader()
			if err != nil {
				_, ok := err.(*websocket.CloseError)
				if ok {
					log.Printf("WebSocket: Read close error for %s: %v", w.RemoteAddr(), err)
					return 0, io.EOF
				}
				if err.Error() == "use of closed network connection" {
					log.Printf("WebSocket: Connection closed for %s", w.RemoteAddr())
					return 0, io.EOF
				}
				log.Printf("WebSocket: Read error for %s: %v", w.RemoteAddr(), err)
				return 0, err
			}

			log.Printf("WebSocket: Received message type %d from %s", messageType, w.RemoteAddr())
			if messageType != websocket.BinaryMessage {
				log.Printf("WebSocket: Received non-binary message from %s", w.RemoteAddr())
				return 0, fmt.Errorf("non-binary message received")
			}
			w.reader = reader
		}

		// 从当前消息中读取数据到缓冲区
		_, err = io.Copy(&w.readBuf, w.reader)
		w.reader = nil
		if err != nil {
			log.Printf("WebSocket: Error reading message to buffer from %s: %v", w.RemoteAddr(), err)
			return 0, err
		}

		// 如果收集到了完整的消息，从缓冲区读取数据
		if w.readBuf.Len() > 0 {
			n, err = w.readBuf.Read(p)
			log.Printf("WebSocket: Read %d bytes from buffer for %s", n, w.RemoteAddr())
			return n, err
		}
	}
}

// Write implements io.Writer interface
func (w *WebSocketConn) Write(p []byte) (n int, err error) {
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

// startHeartbeat starts the heartbeat goroutine
func (w *WebSocketConn) startHeartbeat() {
	ticker := time.NewTicker(30 * time.Second) // 每30秒发送一次ping
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := w.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(10*time.Second)); err != nil {
				log.Printf("WebSocket: Failed to send ping to %s: %v", w.RemoteAddr(), err)
				w.Close()
				return
			}
			log.Printf("WebSocket: Sent ping to %s", w.RemoteAddr())
		case <-w.closeChan:
			return
		}
	}
}

// Close implements io.Closer interface
func (w *WebSocketConn) Close() error {
	var err error
	w.closeOnce.Do(func() {
		close(w.closeChan)
		// 发送关闭帧
		deadline := time.Now().Add(10 * time.Second)
		msg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
		err = w.WriteControl(websocket.CloseMessage, msg, deadline)
		if err != nil {
			log.Printf("WebSocket: Failed to send close frame to %s: %v", w.RemoteAddr(), err)
		}
		err = w.Close()
		log.Printf("WebSocket: Connection closed for %s", w.RemoteAddr())
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

// ServeHTTP implements http.Handler interface
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 打印请求头信息以便调试
	log.Printf("WebSocket: Incoming connection request from %s", r.RemoteAddr)
	log.Printf("WebSocket: Request headers: %v", r.Header)

	// 验证请求的子协议
	subprotocol := ""
	for _, proto := range []string{"mqttv3.1", "mqtt"} {
		if proto == r.Header.Get("Sec-WebSocket-Protocol") {
			subprotocol = proto
			break
		}
	}
	if subprotocol == "" {
		log.Printf("WebSocket: Client did not request a supported protocol")
	} else {
		log.Printf("WebSocket: Using protocol: %s", subprotocol)
	}

	// 使用自定义响应头
	responseHeader := http.Header{}
	if subprotocol != "" {
		responseHeader.Set("Sec-WebSocket-Protocol", subprotocol)
	}

	// 升级 HTTP 连接为 WebSocket 连接
	wsConn, err := wsUpgrader.Upgrade(w, r, responseHeader)
	if err != nil {
		log.Printf("WebSocket: Upgrade error from %s: %v", r.RemoteAddr, err)
		return
	}

	// 设置WebSocket连接选项
	wsConn.SetReadLimit(1024 * 1024) // 1MB
	// 使用更长的超时时间
	wsConn.SetReadDeadline(time.Now().Add(5 * time.Minute))   // 5分钟读超时
	wsConn.SetWriteDeadline(time.Now().Add(30 * time.Second)) // 30秒写超时

	log.Printf("WebSocket: Successfully upgraded connection for %s with protocol %s", r.RemoteAddr, subprotocol)

	// 创建 WebSocket 连接的包装器
	conn := NewWebSocketConn(wsConn)

	// 使用现有的 MQTT 服务器处理该连接
	client := s.newIncomingConn(conn)
	log.Printf("WebSocket: Created MQTT client for %s", r.RemoteAddr)

	client.start()
	log.Printf("WebSocket: Started MQTT client processing for %s", r.RemoteAddr)
}
