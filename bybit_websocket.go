package bybit_connector

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type MessageHandler func(message string) error

func (b *WebSocket) handleIncomingMessages() {
	for {
		_, message, err := b.conn.ReadMessage()
		if err != nil {
			fmt.Println("Error reading:", err)
			b.isConnected = false
			// stop ping for this connection only
			if b.connCancel != nil {
				b.connCancel()
			}
			// best-effort close to unblock any writers
			if b.conn != nil {
				_ = b.conn.Close()
				b.conn = nil
			}
			return
		}

		if b.onMessage != nil {
			err := b.onMessage(string(message))
			if err != nil {
				fmt.Println("Error handling message:", err)
				// stop ping for this connection only
				if b.connCancel != nil {
					b.connCancel()
				}
				return
			}
		}
	}
}

func (b *WebSocket) monitorConnection() {
	ticker := time.NewTicker(time.Second * 5)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if !b.isConnected {
				fmt.Println("Attempting to reconnect...")
				con := b.Connect()
				if con == nil {
					fmt.Println("Reconnection failed:")
				} else {
					fmt.Println("Reconnected.")
				}
			}
		case <-b.lifecycleCtx.Done():
			return
		}
	}
}

func (b *WebSocket) SetMessageHandler(handler MessageHandler) {
	b.onMessage = handler
}

type WebSocket struct {
	conn         *websocket.Conn
	url          string
	apiKey       string
	apiSecret    string
	maxAliveTime string
	pingInterval int
	onMessage    MessageHandler
	// ctx/cancel now split into lifecycle and per-connection contexts:
	// ctx          context.Context
	// cancel       context.CancelFunc
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc
	connCtx         context.Context
	connCancel      context.CancelFunc
	isConnected     bool

	// serialize all writes to the websocket
	writeMu     sync.Mutex
	monitorOnce sync.Once
}

type WebsocketOption func(*WebSocket)

func WithPingInterval(pingInterval int) WebsocketOption {
	return func(c *WebSocket) {
		c.pingInterval = pingInterval
	}
}

func WithMaxAliveTime(maxAliveTime string) WebsocketOption {
	return func(c *WebSocket) {
		c.maxAliveTime = maxAliveTime
	}
}

func NewBybitPrivateWebSocket(url, apiKey, apiSecret string, handler MessageHandler, options ...WebsocketOption) *WebSocket {
	c := &WebSocket{
		url:          url,
		apiKey:       apiKey,
		apiSecret:    apiSecret,
		maxAliveTime: "",
		pingInterval: 20,
		onMessage:    handler,
	}

	// Apply the provided options
	for _, opt := range options {
		opt(c)
	}

	return c
}

func NewBybitPublicWebSocket(url string, handler MessageHandler) *WebSocket {
	c := &WebSocket{
		url:          url,
		pingInterval: 20, // default is 20 seconds
		onMessage:    handler,
	}

	return c
}

func (b *WebSocket) Connect() *WebSocket {
	var err error

	// ensure lifecycle context exists (cancelled only by Disconnect)
	if b.lifecycleCtx == nil || b.lifecycleCtx.Err() != nil {
		b.lifecycleCtx, b.lifecycleCancel = context.WithCancel(context.Background())
	}

	// cancel previous per-connection context (stops old ping)
	if b.connCancel != nil {
		b.connCancel()
	}
	// new per-connection context
	b.connCtx, b.connCancel = context.WithCancel(b.lifecycleCtx)

	wssUrl := b.url
	if b.maxAliveTime != "" {
		wssUrl += "?max_alive_time=" + b.maxAliveTime
	}
	b.conn, _, err = websocket.DefaultDialer.Dial(wssUrl, nil)
	if err != nil {
		fmt.Println("Dial failed:", err)
		return nil
	}

	// Mark as connected so send() can write auth
	b.isConnected = true

	if b.requiresAuthentication() {
		if err = b.sendAuth(); err != nil {
			fmt.Println("Failed Connection:", fmt.Sprintf("%v", err))
			// revert connection state on auth failure
			b.isConnected = false
			_ = b.conn.Close()
			b.conn = nil
			return nil
		}
	}

	// start goroutines
	go b.handleIncomingMessages()
	// start reconnection monitor only once for lifecycle
	b.monitorOnce.Do(func() { go b.monitorConnection() })
	go ping(b)

	return b
}

func (b *WebSocket) SendSubscription(args []string) (*WebSocket, error) {
	reqID := uuid.New().String()
	subMessage := map[string]interface{}{
		"req_id": reqID,
		"op":     "subscribe",
		"args":   args,
	}
	fmt.Println("subscribe msg:", fmt.Sprintf("%v", subMessage["args"]))
	if err := b.sendAsJson(subMessage); err != nil {
		fmt.Println("Failed to send subscription:", err)
		return b, err
	}
	fmt.Println("Subscription sent successfully.")
	return b, nil
}

// SendRequest sendRequest sends a custom request over the WebSocket connection.
func (b *WebSocket) SendRequest(op string, args map[string]interface{}, headers map[string]string, reqId ...string) (*WebSocket, error) {
	finalReqId := uuid.New().String()
	if len(reqId) > 0 && reqId[0] != "" {
		finalReqId = reqId[0]
	}

	request := map[string]interface{}{
		"reqId":  finalReqId,
		"header": headers,
		"op":     op,
		"args":   []interface{}{args},
	}
	fmt.Println("request headers:", fmt.Sprintf("%v", request["header"]))
	fmt.Println("request op channel:", fmt.Sprintf("%v", request["op"]))
	fmt.Println("request msg:", fmt.Sprintf("%v", request["args"]))
	if err := b.sendAsJson(request); err != nil {
		fmt.Println("Failed to send websocket trade request:", err)
		return b, err
	}
	fmt.Println("Successfully sent websocket trade request.")
	return b, nil
}

func (b *WebSocket) SendTradeRequest(tradeTruest map[string]interface{}) (*WebSocket, error) {
	fmt.Println("trade request headers:", fmt.Sprintf("%v", tradeTruest["header"]))
	fmt.Println("trade request op channel:", fmt.Sprintf("%v", tradeTruest["op"]))
	fmt.Println("trade request msg:", fmt.Sprintf("%v", tradeTruest["args"]))
	if err := b.sendAsJson(tradeTruest); err != nil {
		fmt.Println("Failed to send websocket trade request:", err)
		return b, err
	}
	fmt.Println("Successfully sent websocket trade request.")
	return b, nil
}

func ping(b *WebSocket) {
	if b.pingInterval <= 0 {
		fmt.Println("Ping interval is set to a non-positive value.")
		return
	}

	ticker := time.NewTicker(time.Duration(b.pingInterval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// skip if not connected
			if b.conn == nil || !b.isConnected {
				return
			}
			currentTime := time.Now().Unix()
			pingMessage := map[string]string{
				"op":     "ping",
				"req_id": fmt.Sprintf("%d", currentTime),
			}
			jsonPingMessage, err := json.Marshal(pingMessage)
			if err != nil {
				fmt.Println("Failed to marshal ping message:", err)
				continue
			}

			// serialize writes and set a write deadline
			b.writeMu.Lock()
			if b.conn != nil {
				_ = b.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			}
			err = func() error {
				if b.conn == nil || !b.isConnected {
					return fmt.Errorf("connection not available for ping")
				}
				return b.conn.WriteMessage(websocket.TextMessage, jsonPingMessage)
			}()
			b.writeMu.Unlock()

			if err != nil {
				fmt.Println("Failed to send ping:", err)
				// exit to allow reconnection logic to recreate ping
				return
			}
			fmt.Println("Ping sent with UTC time:", currentTime)

		case <-b.connCtx.Done():
			fmt.Println("Ping connection context closed, stopping ping.")
			return
		}
	}
}

func (b *WebSocket) Disconnect() error {
	// stop lifecycle (monitor + future reconnects)
	if b.lifecycleCancel != nil {
		b.lifecycleCancel()
	}
	// stop current connection goroutines (ping)
	if b.connCancel != nil {
		b.connCancel()
	}
	b.isConnected = false
	// serialize close vs. concurrent writers
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if b.conn != nil {
		err := b.conn.Close()
		b.conn = nil
		return err
	}
	return nil
}

func (b *WebSocket) requiresAuthentication() bool {
	return b.url == WEBSOCKET_PRIVATE_MAINNET || b.url == WEBSOCKET_PRIVATE_TESTNET ||
		b.url == WEBSOCKET_TRADE_MAINNET || b.url == WEBSOCKET_TRADE_TESTNET ||
		b.url == WEBSOCKET_TRADE_DEMO || b.url == WEBSOCKET_PRIVATE_DEMO
	// v3 offline
	/*
		b.url == V3_CONTRACT_PRIVATE ||
			b.url == V3_UNIFIED_PRIVATE ||
			b.url == V3_SPOT_PRIVATE
	*/
}

func (b *WebSocket) sendAuth() error {
	// Get current Unix time in milliseconds
	expires := time.Now().UnixNano()/1e6 + 10000
	val := fmt.Sprintf("GET/realtime%d", expires)

	h := hmac.New(sha256.New, []byte(b.apiSecret))
	h.Write([]byte(val))

	// Convert to hexadecimal instead of base64
	signature := hex.EncodeToString(h.Sum(nil))
	fmt.Println("signature generated : " + signature)

	authMessage := map[string]interface{}{
		"req_id": uuid.New(),
		"op":     "auth",
		"args":   []interface{}{b.apiKey, expires, signature},
	}
	fmt.Println("auth args:", fmt.Sprintf("%v", authMessage["args"]))
	return b.sendAsJson(authMessage)
}

func (b *WebSocket) sendAsJson(v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return b.send(string(data))
}

func (b *WebSocket) send(message string) error {
	// serialize all writes and set a write deadline
	b.writeMu.Lock()
	defer b.writeMu.Unlock()

	if b.conn == nil || !b.isConnected {
		return fmt.Errorf("websocket not connected")
	}
	_ = b.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return b.conn.WriteMessage(websocket.TextMessage, []byte(message))
}
