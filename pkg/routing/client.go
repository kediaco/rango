package routing

import (
	"bytes"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	msg "github.com/openware/rango/pkg/message"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second

	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from peer.
	maxMessageSize = 512
)

var (
	newline = []byte{'\n'}
	space   = []byte{' '}
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

type IClient interface {
	Send(string)
	Close()
	GetUID() string
	GetSubscriptions() []string
	SubscribePublic(string)
	SubscribePrivate(string)
	UnsubscribePublic(string)
	UnsubscribePrivate(string)
}

// Client is a middleman between the websocket connection and the hub.
type Client struct {
	hub *Hub

	// User ID if authorized
	UID string

	pubSub  []string
	privSub []string

	// The websocket connection.
	conn *websocket.Conn

	// Buffered channel of outbound messages.
	send   chan []byte
	closed bool
}

// NewClient handles websocket requests from the peer.
func NewClient(hub *Hub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error().Msg("Websocket upgrade failed: " + err.Error())
		return
	}
	client := &Client{
		hub:     hub,
		conn:    conn,
		send:    make(chan []byte, 256),
		closed:  false,
		UID:     r.Header.Get("JwtUID"),
		pubSub:  []string{},
		privSub: []string{},
	}

	if client.UID == "" {
		log.Info().Msgf("New anonymous connection")
	} else {
		log.Info().Msgf("New authenticated connection: %s", client.UID)
	}

	hub.handleSubscribe(&Request{
		client: client,
		Request: msg.Request{
			Streams: parseStreamsFromURI(r.RequestURI),
		},
	})

	// Allow collection of memory referenced by the caller by doing all work in
	// new goroutines.
	go client.write()
	go client.read()
}

func (c *Client) Send(s string) {
	if !c.closed {
		c.send <- []byte(s)
	}
}

func (c *Client) Close() {
	c.closed = true
	close(c.send)
}

func (c *Client) GetUID() string {
	return c.UID
}

func (c *Client) GetSubscriptions() []string {
	return append(c.pubSub, c.privSub...)
}

func (c *Client) SubscribePublic(s string) {
	if !contains(c.pubSub, s) {
		c.pubSub = append(c.pubSub, s)
	}
}

func (c *Client) SubscribePrivate(s string) {
	if !contains(c.privSub, s) {
		c.privSub = append(c.privSub, s)
	}
}

func (c *Client) UnsubscribePublic(s string) {
	l := make([]string, len(c.pubSub)-1)
	i := 0
	for _, el := range c.pubSub {
		if s != el {
			l[i] = el
			i++
		}
	}
	c.pubSub = l
}

func (c *Client) UnsubscribePrivate(s string) {
	l := make([]string, len(c.privSub)-1)
	i := 0
	for _, el := range c.privSub {
		if s != el {
			l[i] = el
			i++
		}
	}
	c.privSub = l
}

func parseStreamsFromURI(uri string) []string {
	streams := make([]string, 0)
	path := strings.Split(uri, "?")
	if len(path) != 2 {
		return streams
	}
	for _, up := range strings.Split(path[1], "&") {
		p := strings.Split(up, "=")
		if len(p) != 2 || p[0] != "stream" {
			continue
		}
		streams = append(streams, strings.Split(p[1], ",")...)

	}
	return streams
}

// read pumps messages from the websocket connection to the hub.
//
// The application runs read in a per-connection goroutine. The application
// ensures that there is at most one reader on a connection by executing all
// reads from this goroutine.
func (c *Client) read() {
	defer func() {
		c.hub.Unregister <- c
		c.conn.Close()
	}()
	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Info().Msgf("error: %v", err)
			}
			break
		}
		message = bytes.TrimSpace(bytes.Replace(message, newline, space, -1))
		if len(message) == 0 {
			continue
		}
		if log.Logger.GetLevel() <= zerolog.DebugLevel {
			log.Debug().Msgf("Received message %s", message)
		}
		req, err := msg.ParseRequest(message)
		if err != nil {
			c.send <- []byte(responseMust(err, nil))
			continue
		}

		c.hub.Requests <- Request{c, req}
	}
}

// write pumps messages from the hub to the websocket connection.
//
// A goroutine running write is started for each connection. The
// application ensures that there is at most one writer to a connection by
// executing all writes from this goroutine.
func (c *Client) write() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// The hub closed the channel.
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)
			if err := w.Close(); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}