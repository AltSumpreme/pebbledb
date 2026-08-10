package pgwire

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
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
	SCRAMSHA256Auth
)

type Config struct {
	AuthMode AuthMode
	User     string
	Password string
	// SCRAMVerifier is a PostgreSQL-compatible SCRAM-SHA-256 verifier. Set
	// either Password or SCRAMVerifier in SCRAM mode, never both.
	SCRAMVerifier   string
	ServerVersion   string
	MaxMessageBytes int
	TLSConfig       *tls.Config
	// AllowInsecurePasswords permits cleartext-password authentication without
	// TLS. It exists for isolated development tests and is unsafe on a network.
	AllowInsecurePasswords bool
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
	scram       *scramCredential
}

func New(database *engine.Engine, config Config) (*Server, error) {
	if database == nil {
		return nil, fmt.Errorf("pgwire: SQL engine is required")
	}
	config = config.normalized()
	if config.AuthMode != TrustAuth && config.AuthMode != CleartextPasswordAuth && config.AuthMode != SCRAMSHA256Auth {
		return nil, fmt.Errorf("pgwire: unsupported authentication mode")
	}
	if config.AuthMode == CleartextPasswordAuth && (config.User == "" || config.Password == "") {
		return nil, fmt.Errorf("pgwire: password authentication requires a non-empty user and password")
	}
	var scram *scramCredential
	if config.AuthMode == SCRAMSHA256Auth {
		if config.User == "" || (config.Password == "") == (config.SCRAMVerifier == "") {
			return nil, fmt.Errorf("pgwire: SCRAM requires a user and exactly one password or verifier")
		}
		var err error
		if config.SCRAMVerifier != "" {
			scram, err = parseSCRAMVerifier(config.SCRAMVerifier)
		} else {
			scram, err = newSCRAMCredential(config.Password, defaultSCRAMIterations)
		}
		if err != nil {
			return nil, err
		}
		config.Password = ""
	}
	if config.AuthMode == CleartextPasswordAuth && config.TLSConfig == nil && !config.AllowInsecurePasswords {
		return nil, fmt.Errorf("pgwire: cleartext-password authentication requires TLS")
	}
	if config.TLSConfig != nil {
		config.TLSConfig = config.TLSConfig.Clone()
		if len(config.TLSConfig.Certificates) == 0 && config.TLSConfig.GetCertificate == nil && config.TLSConfig.GetConfigForClient == nil {
			return nil, fmt.Errorf("pgwire: TLS requires a server certificate or certificate callback")
		}
		if config.TLSConfig.MinVersion == 0 {
			config.TLSConfig.MinVersion = tls.VersionTLS12
		}
	}
	server := &Server{engine: database, config: config, clients: make(map[uint32]*client), scram: scram}
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
	tlsActive := false
	for {
		code, startup, err := readStartup(reader, server.config.MaxMessageBytes)
		if err != nil {
			_ = connection.Close()
			return
		}
		switch code {
		case sslRequestCode:
			if tlsActive {
				_ = writeError(writer, "08P01", fmt.Errorf("TLS is already active"))
				_ = connection.Close()
				return
			}
			if server.config.TLSConfig == nil {
				_, _ = connection.Write([]byte{'N'})
				continue
			}
			if _, err := connection.Write([]byte{'S'}); err != nil {
				_ = connection.Close()
				return
			}
			secured := tls.Server(connection, server.config.TLSConfig)
			if err := secured.Handshake(); err != nil {
				_ = connection.Close()
				return
			}
			connection = secured
			reader, writer = bufio.NewReader(connection), bufio.NewWriter(connection)
			tlsActive = true
			continue
		case cancelRequestCode:
			server.handleCancel(startup)
			_ = connection.Close()
			return
		case protocolVersion3:
			if server.config.AuthMode == CleartextPasswordAuth && !tlsActive && !server.config.AllowInsecurePasswords {
				_ = writeError(writer, "28000", fmt.Errorf("password authentication requires TLS"))
				_ = connection.Close()
				return
			}
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
	if server.config.AuthMode == SCRAMSHA256Auth {
		return server.authenticateSCRAM(reader, writer)
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
