package websocket_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/matryer/is"
	ws "github.com/rmc-floater/websocket"
)

func TestWSHandler(t *testing.T) {

	is := is.New(t)
	ctx := context.Background()

	var p ws.Client
	reg := make(chan ws.Client)
	dereg := make(chan ws.Client)
	manager := ws.NewManager()
	upgrader := websocket.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 1024}
	upgrader.CheckOrigin = func(r *http.Request) bool { return true }
	testBytes := []byte("testing")

	// run the manager
	go manager.Run(ctx)

	// setup the handler
	h := ws.ServeWS(
		upgrader,
		ws.DefaultSetupConn,
		ws.NewClient,
		func(_p ws.Client) {
			p = _p
			manager.RegisterClient(p)
			reg <- p
		},
		func(_p ws.Client) {
			manager.UnregisterClient(_p)
			dereg <- _p
		},
		50*time.Second,
		[]func([]byte){func(b []byte) { p.Write(b) }},
	)

	// create test server
	s := httptest.NewServer(h)
	defer s.Close()

	// connect to the server
	client, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(s.URL, "http"), nil)
	is.NoErr(err)
	defer client.Close()

	// the manager should have one registered client
	<-reg
	is.Equal(len(manager.Clients()), 1)

	// write a message to the server; this will be echoed back
	err = client.WriteMessage(websocket.TextMessage, testBytes)
	is.NoErr(err)

	// server should have echoed the message back
	_, msg, err := client.ReadMessage()
	is.NoErr(err)
	is.Equal(msg, testBytes)

	// close the connection, this should trigger the server/handler to
	// cleanup and unregister the client connection
	client.WriteControl(websocket.CloseMessage, []byte{}, time.Now())
	client.Close()

	// block until the unregistration loop has finished, then assert
	// that the deregistration worked as expected
	_p := <-dereg
	is.Equal(len(manager.Clients()), 0)
	is.Equal(_p, p)
}
