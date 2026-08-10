package pgwire

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"

	"pebbledb/sql/engine"
)

type AuthMode uint8

const (
	TrustAuth AuthMode = iota + 1
	CleartextPasswordAuth
)

type Config struct {
	AuthMode        AuthMode
	User            string
	Password        string
	ServerVersion   string
	MaxMessageBytes int
}

func (config Config) normalized() Config {
	if config.AuthMode == 0 {
		config.AuthMode = TrustAuth
	}
	if config.ServerVersion == "" {
		config.ServerVersion = "15.0-pebbledb"
	}
	if config.MaxMessageBytes <= 0 {
		config.MaxMessageBytes = defaultMaxMessage
	}
	return config
}

type Server struct {
	engine *engine.Engine
	config Config

	mu       sync.Mutex
	listener net.Listener
	clients  map[uint32]*client
	closed   bool
	wait     sync.WaitGroup
	nextPID  atomic.Uint32

	executionMu sync.Mutex
	txOwner     uint32
}

func New(database *engine.Engine, config Config) (*Server, error) {
	if database == nil {
		return nil, fmt.Errorf("pgwire: SQL engine is required")
	}
	config = config.normalized()
	if config.AuthMode != TrustAuth && config.AuthMode != CleartextPasswordAuth {
		return nil, fmt.Errorf("pgwire: unsupported authentication mode")
	}
	server := &Server{engine: database, config: config, clients: make(map[uint32]*client)}
	server.nextPID.Store(1000)
	return server, nil
}

// Serve accepts PostgreSQL v3 connections until Shutdown closes listener.
func (server *Server) Serve(listener net.Listener) error {
	server.mu.Lock()
	if server.listener != nil || server.closed {
		server.mu.Unlock()
		return fmt.Errorf("pgwire: server is already serving or closed")
	}
	server.listener = listener
	server.mu.Unlock()
	for {
		connection, err := listener.Accept()
		if err != nil {
			server.mu.Lock()
			closed := server.closed
			server.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		server.wait.Add(1)
		go func() {
			defer server.wait.Done()
			server.serveConnection(connection)
		}()
	}
}

func (server *Server) Shutdown(ctx context.Context) error {
	server.mu.Lock()
	if !server.closed {
		server.closed = true
		if server.listener != nil {
			_ = server.listener.Close()
		}
		for _, current := range server.clients {
			_ = current.connection.Close()
		}
	}
	server.mu.Unlock()
	done := make(chan struct{})
	go func() { server.wait.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (server *Server) serveConnection(connection net.Conn) {
	reader, writer := bufio.NewReader(connection), bufio.NewWriter(connection)
	for {
		code, startup, err := readStartup(reader, server.config.MaxMessageBytes)
		if err != nil {
			_ = connection.Close()
			return
		}
		switch code {
		case sslRequestCode:
			_, _ = connection.Write([]byte{'N'})
			continue
		case cancelRequestCode:
			server.handleCancel(startup)
			_ = connection.Close()
			return
		case protocolVersion3:
			parameters, err := parseStartupParameters(startup)
			if err != nil {
				_ = writeError(writer, "08P01", err)
				_ = connection.Close()
				return
			}
			if err := server.authenticate(reader, writer, parameters); err != nil {
				_ = writeError(writer, "28P01", err)
				_ = connection.Close()
				return
			}
			client := server.register(connection, reader, writer, parameters)
			defer server.unregister(client)
			if err := client.startupComplete(); err != nil {
				return
			}
			client.loop()
			return
		default:
			_ = writeError(writer, "0A000", fmt.Errorf("unsupported protocol version %d", code))
			_ = connection.Close()
			return
		}
	}
}

func (server *Server) authenticate(reader *bufio.Reader, writer *bufio.Writer, parameters map[string]string) error {
	if server.config.User != "" && parameters["user"] != server.config.User {
		return fmt.Errorf("password authentication failed for user %q", parameters["user"])
	}
	if server.config.AuthMode == TrustAuth {
		return writeMessage(writer, 'R', authentication(0))
	}
	if err := writeMessage(writer, 'R', authentication(3)); err != nil {
		return err
	}
	typeByte, payload, err := readMessage(reader, server.config.MaxMessageBytes)
	if err != nil {
		return err
	}
	if typeByte != 'p' {
		return fmt.Errorf("expected password message")
	}
	password := strings.TrimSuffix(string(payload), "\x00")
	if subtle.ConstantTimeCompare([]byte(password), []byte(server.config.Password)) != 1 {
		return fmt.Errorf("password authentication failed for user %q", parameters["user"])
	}
	return writeMessage(writer, 'R', authentication(0))
}

func (server *Server) register(connection net.Conn, reader *bufio.Reader, writer *bufio.Writer, parameters map[string]string) *client {
	pid := server.nextPID.Add(1)
	var secretRaw [4]byte
	_, _ = rand.Read(secretRaw[:])
	client := &client{
		server: server, connection: connection, reader: reader, writer: writer,
		pid: pid, secret: binary.BigEndian.Uint32(secretRaw[:]), parameters: parameters,
		prepared: make(map[string]preparedStatement), portals: make(map[string]*portal),
	}
	server.mu.Lock()
	server.clients[pid] = client
	server.mu.Unlock()
	return client
}

func (server *Server) unregister(client *client) {
	client.cancelQuery()
	server.mu.Lock()
	delete(server.clients, client.pid)
	server.mu.Unlock()
	server.executionMu.Lock()
	if server.txOwner == client.pid {
		_, _ = server.engine.Execute("ROLLBACK")
		server.txOwner = 0
	}
	server.executionMu.Unlock()
	_ = client.connection.Close()
}

func (server *Server) handleCancel(payload []byte) {
	if len(payload) != 8 {
		return
	}
	pid, secret := binary.BigEndian.Uint32(payload[:4]), binary.BigEndian.Uint32(payload[4:])
	server.mu.Lock()
	client := server.clients[pid]
	server.mu.Unlock()
	if client != nil && client.secret == secret {
		client.cancelQuery()
	}
}

func (server *Server) execute(client *client, ctx context.Context, sql string) ([]result, error) {
	server.executionMu.Lock()
	defer server.executionMu.Unlock()
	if server.txOwner != 0 && server.txOwner != client.pid {
		return nil, fmt.Errorf("another session currently owns an explicit transaction")
	}
	executed, err := server.engine.ExecuteContext(ctx, sql)
	results := make([]result, len(executed))
	for index := range executed {
		results[index] = result{value: executed[index]}
		switch executed[index].Message {
		case "BEGIN":
			server.txOwner, client.inTransaction = client.pid, true
		case "COMMIT", "ROLLBACK":
			server.txOwner, client.inTransaction, client.failedTransaction = 0, false, false
		}
	}
	if err != nil && client.inTransaction {
		client.failedTransaction = true
	}
	normalizedSQL := strings.ToUpper(strings.TrimSpace(sql))
	if err != nil && (strings.HasPrefix(normalizedSQL, "COMMIT") || strings.HasPrefix(normalizedSQL, "ROLLBACK")) {
		server.txOwner, client.inTransaction, client.failedTransaction = 0, false, false
	}
	return results, err
}

func (server *Server) describe(client *client, sql string) ([]column, error) {
	server.executionMu.Lock()
	defer server.executionMu.Unlock()
	if server.txOwner != 0 && server.txOwner != client.pid {
		return nil, fmt.Errorf("another session currently owns an explicit transaction")
	}
	columns, err := server.engine.Describe(sql)
	if err != nil {
		return nil, err
	}
	result := make([]column, len(columns))
	for index := range columns {
		result[index] = column{value: columns[index]}
	}
	return result, nil
}

func writeError(writer *bufio.Writer, code string, err error) error {
	var payload []byte
	payload = append(payload, 'S')
	payload = append(payload, cstring("ERROR")...)
	payload = append(payload, 'C')
	payload = append(payload, cstring(code)...)
	payload = append(payload, 'M')
	payload = append(payload, cstring(err.Error())...)
	payload = append(payload, 0)
	return writeMessage(writer, 'E', payload)
}

func isDisconnect(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed)
}
