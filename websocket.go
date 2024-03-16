package websocket

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// DefaultSetupClient is an example implementation of a function that sets up a
// websocket connection.
func DefaultSetupConn(c *websocket.Conn) {
	pw := 60 * time.Second
	c.SetReadLimit(512)
	c.SetReadDeadline(time.Now().Add(pw))
	c.SetPongHandler(func(string) error {
		c.SetReadDeadline(time.Now().Add(pw))
		return nil
	})
}

// Client is an interface for reading from and writing to a websocket
// connection. It is designed to be used as a middleman between a service and a
// client websocket connection.
type Client interface {
	io.Writer
	io.Closer

	// WritePump is responsible for writing message to the client (including
	// ping message)
	WritePump(func(Client), time.Duration)

	// ReadPump is responsible for setting up the client connection, reading
	// connections from the client, and passing them to the handlers
	ReadPump(func(Client), ...func([]byte))
}

// ServeWS upgrades HTTP connections to WebSocket, creates the pump, calls the
// registration callback, and starts goroutines that handle reading (writing)
// from (to) the client.
func ServeWS(
	// upgrader upgrades the connection
	upgrader websocket.Upgrader,
	// connSetup is called on the upgraded WebSocket connection to configure
	// the connection
	connSetup func(*websocket.Conn),
	// factory is a function that takes a connection and returns a Client
	factory func(*websocket.Conn) Client,
	// register is a function to call once the Client is created (e.g.,
	// store it in a some collection on the service for later reference)
	register func(Client),
	// unregister is a function to call after the WebSocket connection is closed
	// (e.g., remove it from the collection on the service)
	unregister func(Client),
	// ping is the interval at which ping messages are aren't sent
	ping time.Duration,
	// msgHandlers are callbacks that handle messages received from the client
	msgHandlers []func([]byte),
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			// if Upgrade fails it closes the connection, so just return
			return
		}

		// set connection limits and ping/pong settings
		connSetup(conn)

		// create the interface
		client := factory(conn)

		// call the register callback
		register(client)

		// run writePump in a separate goroutine; all writes will happen here,
		// ensuring only one write on the connection at a time
		go client.WritePump(unregister, ping)

		// run readPump in a separate goroutine; all reads will happen here,
		// ensuring only one reader on the connection at a time
		go client.ReadPump(unregister, msgHandlers...)
	}
}

// This is a convenient example implementation of the Client interface.
type client struct {

	// the underlying websocket connection
	conn *websocket.Conn

	// Egress is a buffered channel for writes to the client; callers can push
	// messages into this channel and they'll be written to the client
	egress chan []byte
}

// NewClient is a convenience function for returning a new Client
func NewClient(c *websocket.Conn) Client {
	return &client{
		conn:   c,
		egress: make(chan []byte, 32),
	}
}

// Write implements the Writer interface
func (c *client) Write(p []byte) (int, error) {
	c.egress <- p
	return len(p), nil
}

// Close implements the Closer interface. Note the behavior of calling Close()
// multiple times is undefined, so we're just going to swallow all errors for
// now
func (c *client) Close() error {
	c.conn.WriteControl(websocket.CloseMessage, []byte{}, time.Time{})
	c.conn.Close()
	return nil
}

// WritePump pumps messages from the egress channel (typically originating from
// the service) to the underlying websocket connection.
//
// A goroutine running WritePump is started for each connection. The application
// ensures that there is at most one writer to a connection by executing all
// writes from this goroutine.
func (c *client) WritePump(unregisterFunc func(Client), ping time.Duration) {

	// Create a ticker that triggers a ping at given interval
	pingTicker := time.NewTicker(ping)
	defer func() {
		pingTicker.Stop()
		unregisterFunc(c)
	}()

	for {
		select {
		case msgBytes, ok := <-c.egress:
			// ok will be false in case the egress channel is closed
			if !ok {
				// manager has closed this connection channel, so send a close
				// message and return which will initiate the connection
				// shutdown process in the manager
				c.conn.WriteMessage(websocket.CloseMessage, nil)
				return
			}
			// write a message to the connection
			if err := c.conn.WriteMessage(websocket.TextMessage, msgBytes); err != nil {
				// just return to (closes the connection) indiscriminantly in
				// the case of errors
				return
			}
		case <-pingTicker.C:
			if err := c.conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
				// just return to (closes the connection) indiscriminantly in
				// the case of errors
				return
			}
		}
	}
}

// ReadPump pumps messages from the websocket connection to the service.
//
// The application runs ReadPump in a per-connection goroutine. The application
// ensures that there is at most one reader on a connection by executing all
// reads from this goroutine.
func (c *client) ReadPump(
	unregisterFunc func(Client),
	handlers ...func([]byte),
) {
	// unregister and close before exit
	defer func() {
		unregisterFunc(c)
	}()

	// read forever
	for {
		_, payload, err := c.conn.ReadMessage()

		// handle close (we could check the error here but it doesn't really
		// matter, something is botched, so just close the connection)
		if err != nil {
			break
		}

		// do things with the message
		for _, h := range handlers {
			h(payload)
		}
	}
}

// Manager maintains the set of active WebSocket clients.
type Manager interface {
	// Clients returns a slice of Clients
	Clients() []Client

	// RegisterClient adds the client to the Managers internal store
	RegisterClient(Client)

	// UnregisterClient removes the client from the Managers internal store
	UnregisterClient(Client)

	// Run runs in its own goroutine. It continuously processes client
	// (un)registration. When a client registers, it is added to the manager's
	// internal store. When a client unregisters, it is removed from the
	// manager's internal store.
	Run(context.Context)
}

type manager struct {
	lock       sync.RWMutex
	clients    map[Client]struct{}
	register   chan regreq
	unregister chan regreq
}

// Helper struct for signaling registration/unregistration. The Run() goroutine
// can signal the operation is done by sending on the done chan.
type regreq struct {
	wp   Client
	done chan struct{}
}

func NewManager() Manager {
	return &manager{
		lock:       sync.RWMutex{},
		clients:    make(map[Client]struct{}),
		register:   make(chan regreq),
		unregister: make(chan regreq),
	}
}

func (m *manager) Clients() []Client {
	res := []Client{}

	m.lock.RLock()
	defer m.lock.RUnlock()

	for c, _ := range m.clients {
		res = append(res, c)
	}
	return res
}

func (m *manager) RegisterClient(wp Client) {
	done := make(chan struct{})
	rr := regreq{
		wp:   wp,
		done: done,
	}
	m.register <- rr
	<-done
}

func (m *manager) UnregisterClient(wp Client) {
	done := make(chan struct{})
	rr := regreq{
		wp:   wp,
		done: done,
	}
	m.unregister <- rr
	<-done
}

func (m *manager) Run(ctx context.Context) {

	// helper fn for cleaning up client
	cleanupClient := func(c Client) {
		// delete from map
		delete(m.clients, c)
		// close connections
		c.Close()
	}

	// run forever registering, unregistering, and listening for cleanup
	for {
		select {
		// register new client
		case rr := <-m.register:

			m.lock.Lock()
			m.clients[rr.wp] = struct{}{}
			m.lock.Unlock()
			rr.done <- struct{}{}

		// cleanup single client
		case rr := <-m.unregister:

			m.lock.Lock()
			if _, ok := m.clients[rr.wp]; ok {
				cleanupClient(rr.wp)
			}
			m.lock.Unlock()
			rr.done <- struct{}{}

		// handle service shutdown
		case <-ctx.Done():

			m.lock.Lock()
			for client := range m.clients {
				cleanupClient(client)
			}
			m.lock.Unlock()
		}
	}
}

// Broadcaster is an example implementation of Manager that has a
// Broadcast method that writes the supplied message to all clients.
type Broadcaster struct {
	*manager
}

func NewBroadcaster() Manager {
	m := manager{
		lock:       sync.RWMutex{},
		clients:    make(map[Client]struct{}),
		register:   make(chan regreq),
		unregister: make(chan regreq),
	}
	return &Broadcaster{
		manager: &m,
	}
}

func (bb *Broadcaster) Broadcast(b []byte) {
	bb.lock.RLock()
	defer bb.lock.RUnlock()
	for w := range bb.clients {
		w.Write(b)
	}

}
