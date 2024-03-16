# websocket

I wanted a more concise WebSocket interface than what Gorilla provides out of the box, so I wrote this package.

## Usage

The main entry point for this package is the `ServeWS` function, which returns an `http.HandlerFunc` that will upgrade client connections and subsequently handle reading from and writing to the client. The caller simply needs to supply zero or more callback functions that are invoked on every message received from the client.

## Interfaces

There are two interfaces: `Pumper` and `Manager`. The package provides example implementations, but consumers are free to implement their own.

`Pumper` is responsible for reading (writing) messages from (to) the client and invoking the callbacks on the client messages, which are assumed to be of type `[]byte`.

`Manager` is really more of a convenience interface for managing client connections; you don't actually _need_ to use this, but it's helpful.

## Usage

The tests show a simple usage.
