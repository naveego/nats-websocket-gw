package gw

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nats-io/go-nats"
)

type DialerSettings struct {
	GatewayAddr  string
	Dialer       *websocket.Dialer
	Origin       string
	Cookie       string
	ErrorHandler ErrorHandler
	Tracer       func(string)
}

type dialer struct {
	settings DialerSettings
	tracef   func(string, ...interface{})
	errf     func(string, ...interface{}) error
}

type gwconn struct {
	ws        *websocket.Conn
	cmdWriter *io.PipeWriter
}

// NewGatewayDialer returns a nats.Dialer which will connect to the gateway over websockets.
func NewGatewayDialer(settings DialerSettings) (nats.CustomDialer, error) {
	d := &dialer{
		settings: settings,
	}
	d.errf = func(s string, a ...interface{}) error {
		err := fmt.Errorf(s, a...)
		if settings.ErrorHandler != nil {
			settings.ErrorHandler(err)
		}
		return err
	}
	if settings.Tracer != nil {
		d.tracef = func(s string, a ...interface{}) {
			t := fmt.Sprintf(s, a...)
			settings.Tracer(t)
		}
	} else {
		d.tracef = func(s string, a ...interface{}) {
			return
		}
	}

	return d, nil
}

func (d *dialer) Dial(network, address string) (net.Conn, error) {
	d.tracef("dial(%q, %q)", network, address)

	var wsDialer *websocket.Dialer
	if d.settings.Dialer != nil {
		wsDialer = d.settings.Dialer
	} else {
		wsDialer = &websocket.Dialer{}
	}

	requestHeader := http.Header{}
	if d.settings.Origin != "" {
		requestHeader.Set("Origin", d.settings.Origin)
	}
	if d.settings.Cookie != "" {
		requestHeader.Set("Cookie", d.settings.Cookie)
	}

	ws, resp, err := wsDialer.Dial(d.settings.GatewayAddr, requestHeader)
	if err != nil {
		return nil, d.errf("couldn't connect to %q: %s", d.settings.GatewayAddr, err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, d.errf("HTTP request got response with status code %d", resp.StatusCode)
	}

	cr, cmdWriter := io.Pipe()
	conn := &gwconn{
		ws:        ws,
		cmdWriter: cmdWriter,
	}

	cmdReader := NewCommandsReader(cr)
	go func() {
		for {
			cmd, err := cmdReader.nextCommand()
			if err != nil {
				cmdWriter.CloseWithError(d.errf("error reading next command: %s", err))
				return
			}
			d.tracef("got command: %q", cmd)

			err = ws.WriteMessage(websocket.TextMessage, cmd)
			if err != nil {
				cmdWriter.CloseWithError(d.errf("error writing command to gateway: %s", err))
				cmdWriter.CloseWithError(err)
				return
			}
		}
	}()

	return conn, nil
}

// Read reads data from the connection.
// Read can be made to time out and return an Error with Timeout() == true
// after a fixed time limit; see SetDeadline and SetReadDeadline.
func (c *gwconn) Read(b []byte) (n int, err error) {
	mt, reader, err := c.ws.NextReader()
	if err != nil {
		return 0, err
	}
	if mt != websocket.TextMessage {
		return 0, fmt.Errorf("unexpected message type %d", mt)
	}
	n, err = reader.Read(b)
	return
}

// Write writes data to the connection.
// Write can be made to time out and return an Error with Timeout() == true
// after a fixed time limit; see SetDeadline and SetWriteDeadline.
func (c *gwconn) Write(b []byte) (n int, err error) {

	n, err = c.cmdWriter.Write(b)
	return

}

// Close closes the connection.
// Any blocked Read or Write operations will be unblocked and return errors.
func (c *gwconn) Close() error {
	c.cmdWriter.Close()
	return c.ws.Close()
}

// LocalAddr returns the local network address.
func (c *gwconn) LocalAddr() net.Addr {
	return c.ws.LocalAddr()
}

// RemoteAddr returns the remote network address.
func (c *gwconn) RemoteAddr() net.Addr {
	return c.ws.RemoteAddr()
}

// SetDeadline sets the read and write deadlines associated
// with the connection. It is equivalent to calling both
// SetReadDeadline and SetWriteDeadline.
//
// A deadline is an absolute time after which I/O operations
// fail with a timeout (see type Error) instead of
// blocking. The deadline applies to all future and pending
// I/O, not just the immediately following call to Read or
// Write. After a deadline has been exceeded, the connection
// can be refreshed by setting a deadline in the future.
//
// An idle timeout can be implemented by repeatedly extending
// the deadline after successful Read or Write calls.
//
// A zero value for t means I/O operations will not time out.
func (c *gwconn) SetDeadline(t time.Time) error {
	err := c.ws.SetReadDeadline(t)
	if err == nil {
		err = c.ws.SetWriteDeadline(t)
	}
	return err
}

// SetReadDeadline sets the deadline for future Read calls
// and any currently-blocked Read call.
// A zero value for t means Read will not time out.
func (c *gwconn) SetReadDeadline(t time.Time) error {
	return c.ws.SetReadDeadline(t)
}

// SetWriteDeadline sets the deadline for future Write calls
// and any currently-blocked Write call.
// Even if write times out, it may return n > 0, indicating that
// some of the data was successfully written.
// A zero value for t means Write will not time out.
func (c *gwconn) SetWriteDeadline(t time.Time) error {
	return c.ws.SetWriteDeadline(t)
}
