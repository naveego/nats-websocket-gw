package gw

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/gorilla/websocket"
)

type ErrorHandler func(error)
type ConnectHandler func(*NatsConn, *websocket.Conn) error

// FilterFactory is a function which returns a Filter
// This will be invoked once for each connection. Filter.Reader will
// be called to wrap every read from the websocket, while the Filter.Writer
// will be called to wrap every write to the websocket.
type FilterFactory func(r *http.Request) Filter
type Filter interface {
	Reader(io.Reader) io.Reader
	Writer(io.WriteCloser) io.WriteCloser
}

type NatsServerInfo string

type Settings struct {
	NatsAddr       string
	EnableTls      bool
	TlsConfig      *tls.Config
	ConnectHandler ConnectHandler
	FilterFactory  FilterFactory
	ErrorHandler   ErrorHandler
	WSUpgrader     *websocket.Upgrader
	Trace          bool
}

type Gateway struct {
	settings      Settings
	onError       ErrorHandler
	handleConnect ConnectHandler
	makeFilter    FilterFactory
}

var defaultUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

type NatsConn struct {
	Conn       net.Conn
	CmdReader  CommandsReader
	ServerInfo NatsServerInfo
}

func (gw *Gateway) defaultConnectHandler(natsConn *NatsConn, wsConn *websocket.Conn) error {
	// Default behavior is to let the client on the other side do the CONNECT
	// after having forwarded the 'INFO' command
	infoCmd := append([]byte("INFO "), []byte(natsConn.ServerInfo)...)
	infoCmd = append(infoCmd, byte('\r'), byte('\n'))
	if gw.settings.Trace {
		fmt.Println("[TRACE] <--", string(infoCmd))
	}
	if err := wsConn.WriteMessage(websocket.TextMessage, infoCmd); err != nil {
		return err
	}
	return nil
}

func defaultErrorHandler(err error) {
	fmt.Println("[ERROR]", err)
}

func defaultFilterFactory(r *http.Request) Filter {
	return nil
}

func copyAndTrace(prefix string, dst io.Writer, src io.Reader, buf []byte) (int64, error) {
	read, err := src.Read(buf)
	if err != nil {
		return 0, err
	}
	fmt.Println("[TRACE]", prefix, string(buf[:read]))
	written, err := dst.Write(buf[:read])
	if written != read {
		return int64(written), io.ErrShortWrite
	}
	return int64(written), err
}

func NewGateway(settings Settings) *Gateway {
	gw := Gateway{
		settings: settings,
	}
	gw.setErrorHandler(settings.ErrorHandler)
	gw.setConnectHandler(settings.ConnectHandler)
	gw.setFilterFactory(settings.FilterFactory)
	return &gw
}

func (gw *Gateway) setFilterFactory(factory FilterFactory) {
	if factory == nil {
		gw.makeFilter = defaultFilterFactory
	} else {
		gw.makeFilter = factory
	}
}

func (gw *Gateway) setErrorHandler(handler ErrorHandler) {
	if handler == nil {
		gw.onError = defaultErrorHandler
	} else {
		gw.onError = handler
	}
}

func (gw *Gateway) setConnectHandler(handler ConnectHandler) {
	if handler == nil {
		gw.handleConnect = gw.defaultConnectHandler
	} else {
		gw.handleConnect = handler
	}
}

func (gw *Gateway) natsToWsWorker(ws *websocket.Conn, src CommandsReader, filter Filter, doneCh chan<- bool) {
	defer func() {
		doneCh <- true
	}()

	for {
		cmd, err := src.nextCommand()
		if err != nil {
			gw.onError(err)
			return
		}
		if gw.settings.Trace {
			fmt.Println("[TRACE] <--", string(cmd))
		}
		w, err := ws.NextWriter(websocket.TextMessage)
		if err != nil {
			gw.onError(err)
			return
		}

		if filter != nil {
			w = filter.Writer(w)
		}

		if _, err = w.Write(cmd); err != nil {
			gw.onError(err)
			return
		}
		if err = w.Close(); err != nil {
			gw.onError(err)
		}
	}
}

func (gw *Gateway) wsToNatsWorker(nats net.Conn, ws *websocket.Conn, filter Filter, doneCh chan<- bool) {
	defer func() {
		doneCh <- true
	}()
	var buf []byte
	if gw.settings.Trace {
		buf = make([]byte, 1024*1024)
	}
	for {
		_, src, err := ws.NextReader()
		if err != nil {
			gw.onError(err)
			return
		}

		if filter != nil {
			src = filter.Reader(src)
		}

		if gw.settings.Trace {
			_, err = copyAndTrace("-->", nats, src, buf)
		} else {
			_, err = io.Copy(nats, src)
		}
		if err != nil {
			gw.onError(err)
			return
		}
	}
}

func (gw *Gateway) Handler(w http.ResponseWriter, r *http.Request) {
	upgrader := defaultUpgrader
	if gw.settings.WSUpgrader != nil {
		upgrader = *gw.settings.WSUpgrader
	}
	wsConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		gw.onError(err)
		return
	}
	natsConn, err := gw.initNatsConnectionForWSConn(wsConn)
	if err != nil {
		gw.onError(err)
		return
	}

	doneCh := make(chan bool)

	filter := gw.makeFilter(r)

	go gw.natsToWsWorker(wsConn, natsConn.CmdReader, filter, doneCh)
	go gw.wsToNatsWorker(natsConn.Conn, wsConn, filter, doneCh)

	<-doneCh

	wsConn.Close()
	natsConn.Conn.Close()

	<-doneCh
}

func readInfo(cmd []byte) (NatsServerInfo, error) {
	if !bytes.Equal(cmd[:5], []byte("INFO ")) {
		return "", fmt.Errorf("Invalid 'INFO' command: %s", string(cmd))
	}
	return NatsServerInfo(cmd[5 : len(cmd)-2]), nil
}

// initNatsConnectionForRequest open a connection to the nats server, consume the
// INFO message if needed, and finally handle the CONNECT
func (gw *Gateway) initNatsConnectionForWSConn(wsConn *websocket.Conn) (*NatsConn, error) {
	conn, err := net.Dial("tcp", gw.settings.NatsAddr)
	if err != nil {
		return nil, err
	}
	natsConn := NatsConn{Conn: conn, CmdReader: NewCommandsReader(conn)}

	// read the INFO, keep it
	infoCmd, err := natsConn.CmdReader.nextCommand()
	if err != nil {
		return nil, err
	}

	info, err := readInfo(infoCmd)

	if err != nil {
		return nil, err
	}

	natsConn.ServerInfo = info

	// optionally initialize the TLS layer
	// TODO check if the server requires TLS, which overrides the 'enableTls' setting
	if gw.settings.EnableTls {
		tlsConfig := gw.settings.TlsConfig
		if tlsConfig == nil {
			tlsConfig = &tls.Config{
				InsecureSkipVerify: true,
			}
		}
		tlsConn := tls.Client(conn, tlsConfig)
		tlsConn.Handshake()
		natsConn.Conn = tlsConn
		natsConn.CmdReader = NewCommandsReader(tlsConn)
	}

	if err := gw.handleConnect(&natsConn, wsConn); err != nil {
		return nil, err
	}

	return &natsConn, nil
}
