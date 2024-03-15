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

// WSPumper is an interface for reading from and writing to a websocket
// connection. It is designed to be used as a middleman between a service and a
// client websocket connection.
type WSPumper interface {
	io.Writer
	io.Closer

	// WritePump is responsible for writing message to the client (including
	// ping message)
	WritePump(time.Duration, func(WSPumper))

	// ReadPump is responsible for setting up the client connection, reading
	// connections from the client, and passing them to the handlers
	ReadPump(func(*websocket.Conn), func(WSPumper), ...func([]byte))
}

// ServeWS upgrades HTTP connections to WebSocket, creates the pump, calls the
// registration callback, and starts goroutines that handle reading (writing)
// from (to) the client.
func ServeWS(
	// upgrader upgrades the connection
	upgrader websocket.Upgrader,
	// oc checks the origin and returns "true" for "good" origins
	oc func(*http.Request) bool,
	// factory is a function that takes a connection and returns a WSPumper
	factory func(*websocket.Conn) WSPumper,
	// register is a function to call once the WSPumper is created (e.g.,
	// store it in a some collection on the service for later reference)
	register func(WSPumper),
	// unregister is a function to call after the WebSocket connection is closed
	// (e.g., remove it from the collection on the service)
	unregister func(WSPumper),
	// ping is the interval at which ping messages are aren't sent
	ping time.Duration,
	// connSetup is called on the upgraded WebSocket connection to configure
	// the connection
	connSetup func(*websocket.Conn),
	// msgHandlers are callbacks that handle messages received from the client
	msgHandlers []func([]byte),
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// upgrade
		upgrader.CheckOrigin = oc
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			// if Upgrade fails it closes the connection, so just return
			return
		}

		// create the interface
		p := factory(c)

		// call the register callback
		register(p)

		// run writePump in a separate goroutine; all writes will happen here,
		// ensuring only one write on the connection at a time
		go p.WritePump(ping, unregister)

		// run readPump in a separate goroutine; all reads will happen here,
		// ensuring only one reader on the connection at a time
		go p.ReadPump(connSetup, unregister, msgHandlers...)
	}
}

// This is a convenient example implementation of the WSPumper interface.
type BasicPumper struct {

	// the underlying websocket connection
	conn *websocket.Conn

	// Egress is a buffered channel for writes to the client; callers can push
	// messages into this channel and they'll be written to the client
	egress chan []byte
}

// DefaultPumperFactory is a convenience function for returning a new WSPumper
func DefaultPumperFactory(c *websocket.Conn) WSPumper {
	return &BasicPumper{
		conn:   c,
		egress: make(chan []byte, 32),
	}
}

// Write implements the Writer interface
func (bp *BasicPumper) Write(p []byte) (int, error) {
	bp.egress <- p
	return len(p), nil
}

// Close implements the Closer interface. Note the behavior of calling Close()
// multiple times is undefined, so we're just going to swallow all errors for
// now
func (bp *BasicPumper) Close() error {
	bp.conn.WriteControl(websocket.CloseMessage, []byte{}, time.Time{})
	bp.conn.Close()
	return nil
}

// WritePump pumps messages from the egress channel (typically originating from
// the service) to the underlying websocket connection.
//
// A goroutine running WritePump is started for each connection. The application
// ensures that there is at most one writer to a connection by executing all
// writes from this goroutine.
func (bp *BasicPumper) WritePump(ping time.Duration, unregisterFunc func(WSPumper)) {

	// Create a ticker that triggers a ping at given interval
	pingTicker := time.NewTicker(ping)
	defer func() {
		pingTicker.Stop()
		unregisterFunc(bp)
	}()

	for {
		select {
		case msgBytes, ok := <-bp.egress:
			// ok will be false in case the egress channel is closed
			if !ok {
				// manager has closed this connection channel, so send a close
				// message and return which will initiate the connection
				// shutdown process in the manager
				bp.conn.WriteMessage(websocket.CloseMessage, nil)
				return
			}
			// write a message to the connection
			if err := bp.conn.WriteMessage(websocket.TextMessage, msgBytes); err != nil {
				// just return to (closes the connection) indiscriminantly in
				// the case of errors
				return
			}
		case <-pingTicker.C:
			if err := bp.conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
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
func (bp *BasicPumper) ReadPump(
	setupConn func(*websocket.Conn),
	unregisterFunc func(WSPumper),
	handlers ...func([]byte),
) {
	// unregister and close before exit
	defer func() {
		unregisterFunc(bp)
	}()

	// set connection limits and ping/pong settings
	setupConn(bp.conn)

	// read forever
	for {
		_, payload, err := bp.conn.ReadMessage()

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

// WSManager maintains the set of active WebSocket clients.
type WSManager interface {
	// Clients returns a slice of Clients
	Clients() []WSPumper

	// RegisterClient adds the client to the WSManagers internal store
	RegisterClient(WSPumper)

	// UnregisterClient removes the client from the WSManagers internal store
	UnregisterClient(WSPumper)

	// Run runs in its own goroutine. It continuously processes client
	// (un)registration. When a client registers, it is added to the manager's
	// internal store. When a client unregisters, it is removed from the
	// manager's internal store.
	Run(context.Context)
}

// BasicBroadcaster is an example implementation of WSManager that has a
// Broadcast method that writes the supplied message to all clients.
type BasicBroadcaster struct {
	lock       sync.RWMutex
	clients    map[WSPumper]struct{}
	register   chan regreq
	unregister chan regreq
}

// Helper struct for signaling registration/unregistration. The Run() goroutine
// can signal the operation is done by sending on the done chan.
type regreq struct {
	wp   WSPumper
	done chan struct{}
}

func BasicBroadcasterFactory() WSManager {
	return &BasicBroadcaster{
		lock:       sync.RWMutex{},
		clients:    make(map[WSPumper]struct{}),
		register:   make(chan regreq),
		unregister: make(chan regreq),
	}
}

func (bb *BasicBroadcaster) Clients() []WSPumper {
	res := []WSPumper{}

	bb.lock.RLock()
	defer bb.lock.RUnlock()

	for c, _ := range bb.clients {
		res = append(res, c)
	}
	return res
}

func (bb *BasicBroadcaster) RegisterClient(wp WSPumper) {
	done := make(chan struct{})
	rr := regreq{
		wp:   wp,
		done: done,
	}
	bb.register <- rr
	<-done
}

func (bb *BasicBroadcaster) UnregisterClient(wp WSPumper) {
	done := make(chan struct{})
	rr := regreq{
		wp:   wp,
		done: done,
	}
	bb.unregister <- rr
	<-done
}

func (bb *BasicBroadcaster) Run(ctx context.Context) {

	// helper fn for cleaning up client
	cleanupClient := func(c WSPumper) {
		// delete from map
		delete(bb.clients, c)
		// close connections
		c.Close()
	}

	// run forever registering, unregistering, and listening for cleanup
	for {
		select {
		// register new client
		case rr := <-bb.register:

			bb.lock.Lock()
			bb.clients[rr.wp] = struct{}{}
			bb.lock.Unlock()
			rr.done <- struct{}{}

		// cleanup single client
		case rr := <-bb.unregister:

			bb.lock.Lock()
			if _, ok := bb.clients[rr.wp]; ok {
				cleanupClient(rr.wp)
			}
			bb.lock.Unlock()
			rr.done <- struct{}{}

		// handle service shutdown
		case <-ctx.Done():

			bb.lock.Lock()
			for client := range bb.clients {
				cleanupClient(client)
			}
			bb.lock.Unlock()
		}
	}
}

func (bb *BasicBroadcaster) Broadcast(b []byte) {
	for w := range bb.clients {
		w.Write(b)
	}
}
