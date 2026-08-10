package pgwire

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"pebbledb/sql/engine"
)

func TestSimpleAndExtendedQueryProtocols(t *testing.T) {
	database, server, address := startTestServer(t, Config{AuthMode: TrustAuth})
	defer database.Close()
	connection, reader, writer, startup := connect(t, address, "alice", "")
	defer connection.Close()
	if startup.backendPID == 0 || startup.secret == 0 {
		t.Fatalf("missing backend key: %+v", startup)
	}

	messages := simple(t, reader, writer, `
        CREATE TABLE users (id BIGINT PRIMARY KEY, name TEXT NOT NULL);
        INSERT INTO users VALUES (1, 'Alice'), (2, 'Bob');
        SELECT id, name FROM users ORDER BY id;
    `)
	if countType(messages, 'C') != 3 || countType(messages, 'D') != 2 || messages[len(messages)-1].kind != 'Z' {
		t.Fatalf("simple query messages=%v", messageKinds(messages))
	}
	rows := messagesOfType(messages, 'D')
	if values := decodeTextRow(t, rows[1].payload); values[0] != "2" || values[1] != "Bob" {
		t.Fatalf("second row=%v", values)
	}

	parsePayload := append(cstring("find_user"), cstring("SELECT name FROM users WHERE id = $1")...)
	var parseTypes bytes.Buffer
	parseTypes.Write(parsePayload)
	_ = binary.Write(&parseTypes, binary.BigEndian, int16(1))
	_ = binary.Write(&parseTypes, binary.BigEndian, int8OID)
	sendFrontend(t, writer, 'P', parseTypes.Bytes())
	if message := receive(t, reader); message.kind != '1' {
		t.Fatalf("Parse response=%q", message.kind)
	}

	var bind bytes.Buffer
	bind.Write(cstring("portal1"))
	bind.Write(cstring("find_user"))
	_ = binary.Write(&bind, binary.BigEndian, int16(0)) // text parameter formats
	_ = binary.Write(&bind, binary.BigEndian, int16(1))
	_ = binary.Write(&bind, binary.BigEndian, int32(1))
	bind.WriteByte('2')
	_ = binary.Write(&bind, binary.BigEndian, int16(1))
	_ = binary.Write(&bind, binary.BigEndian, int16(1)) // binary results
	sendFrontend(t, writer, 'B', bind.Bytes())
	if message := receive(t, reader); message.kind != '2' {
		t.Fatalf("Bind response=%q payload=%q", message.kind, message.payload)
	}
	sendFrontend(t, writer, 'D', append([]byte{'P'}, cstring("portal1")...))
	if message := receive(t, reader); message.kind != 'T' {
		t.Fatalf("Describe response=%q", message.kind)
	}
	var execute bytes.Buffer
	execute.Write(cstring("portal1"))
	_ = binary.Write(&execute, binary.BigEndian, int32(0))
	sendFrontend(t, writer, 'E', execute.Bytes())
	data := receive(t, reader)
	if data.kind != 'D' {
		t.Fatalf("Execute first response=%q", data.kind)
	}
	if values := decodeBinaryTextRow(t, data.payload); len(values) != 1 || string(values[0]) != "Bob" {
		t.Fatalf("binary row=%q", values)
	}
	if complete := receive(t, reader); complete.kind != 'C' {
		t.Fatalf("Execute completion=%q", complete.kind)
	}
	sendFrontend(t, writer, 'S', nil)
	if ready := receive(t, reader); ready.kind != 'Z' || string(ready.payload) != "I" {
		t.Fatalf("Sync response=%q %q", ready.kind, ready.payload)
	}

	// A real CancelRequest uses the BackendKeyData pair and has no response.
	canceled := make(chan struct{})
	var cancelOnce sync.Once
	server.mu.Lock()
	client := server.clients[startup.backendPID]
	server.mu.Unlock()
	client.setCancel(func() { cancelOnce.Do(func() { close(canceled) }) })
	cancelConnection, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	var cancelBody bytes.Buffer
	_ = binary.Write(&cancelBody, binary.BigEndian, int32(cancelRequestCode))
	_ = binary.Write(&cancelBody, binary.BigEndian, startup.backendPID)
	_ = binary.Write(&cancelBody, binary.BigEndian, startup.secret)
	var startupCancel bytes.Buffer
	_ = binary.Write(&startupCancel, binary.BigEndian, int32(cancelBody.Len()+4))
	startupCancel.Write(cancelBody.Bytes())
	_, _ = cancelConnection.Write(startupCancel.Bytes())
	_ = cancelConnection.Close()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("CancelRequest did not cancel the matching backend")
	}
}

func TestCleartextAuthenticationAndTransactionStatus(t *testing.T) {
	database, _, address := startTestServer(t, Config{AuthMode: CleartextPasswordAuth, User: "reuben", Password: "secret", AllowInsecurePasswords: true})
	defer database.Close()
	connection, reader, writer, _ := connect(t, address, "reuben", "secret")
	defer connection.Close()
	messages := simple(t, reader, writer, "BEGIN")
	if ready := messages[len(messages)-1]; ready.kind != 'Z' || string(ready.payload) != "T" {
		t.Fatalf("BEGIN ready status=%q", ready.payload)
	}
	messages = simple(t, reader, writer, "ROLLBACK")
	if ready := messages[len(messages)-1]; ready.kind != 'Z' || string(ready.payload) != "I" {
		t.Fatalf("ROLLBACK ready status=%q", ready.payload)
	}

	bad, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	badReader, badWriter := bufio.NewReader(bad), bufio.NewWriter(bad)
	sendStartup(t, badWriter, "reuben")
	if request := receive(t, badReader); request.kind != 'R' || binary.BigEndian.Uint32(request.payload) != 3 {
		t.Fatalf("auth request=%q %v", request.kind, request.payload)
	}
	sendFrontend(t, badWriter, 'p', cstring("wrong"))
	if response := receive(t, badReader); response.kind != 'E' {
		t.Fatalf("bad password response=%q", response.kind)
	}
	_ = bad.Close()
}

func TestPasswordAuthenticationRequiresTLSByDefault(t *testing.T) {
	database, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := New(database, Config{AuthMode: CleartextPasswordAuth, User: "reuben", Password: "secret"}); err == nil {
		t.Fatal("insecure password server unexpectedly configured")
	}
	if _, err := New(database, Config{AuthMode: CleartextPasswordAuth, AllowInsecurePasswords: true}); err == nil {
		t.Fatal("empty password credentials unexpectedly configured")
	}
	if _, err := New(database, Config{TLSConfig: &tls.Config{}}); err == nil {
		t.Fatal("TLS without a certificate unexpectedly configured")
	}
}

func TestTLSNegotiationProtectsPasswordAuthentication(t *testing.T) {
	certificate, root := testTLSCertificate(t)
	database, _, address := startTestServer(t, Config{
		AuthMode:  CleartextPasswordAuth,
		User:      "reuben",
		Password:  "secret",
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{certificate}},
	})
	defer database.Close()
	unsecured, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	unsecuredReader, unsecuredWriter := bufio.NewReader(unsecured), bufio.NewWriter(unsecured)
	sendStartup(t, unsecuredWriter, "reuben")
	if message := receive(t, unsecuredReader); message.kind != 'E' {
		t.Fatalf("unencrypted password startup response = %q, want error", message.kind)
	}
	_ = unsecured.Close()

	plain, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	var request bytes.Buffer
	_ = binary.Write(&request, binary.BigEndian, int32(8))
	_ = binary.Write(&request, binary.BigEndian, int32(sslRequestCode))
	if _, err := plain.Write(request.Bytes()); err != nil {
		t.Fatal(err)
	}
	var response [1]byte
	if _, err := io.ReadFull(plain, response[:]); err != nil {
		t.Fatal(err)
	}
	if response[0] != 'S' {
		t.Fatalf("SSL response = %q, want S", response[0])
	}
	secured := tls.Client(plain, &tls.Config{RootCAs: root, ServerName: "localhost", MinVersion: tls.VersionTLS12})
	if err := secured.Handshake(); err != nil {
		t.Fatal(err)
	}
	defer secured.Close()
	if secured.ConnectionState().Version < tls.VersionTLS12 {
		t.Fatalf("negotiated insecure TLS version %x", secured.ConnectionState().Version)
	}
	reader, writer := bufio.NewReader(secured), bufio.NewWriter(secured)
	sendStartup(t, writer, "reuben")
	for {
		message := receive(t, reader)
		if message.kind == 'R' && binary.BigEndian.Uint32(message.payload) == 3 {
			sendFrontend(t, writer, 'p', cstring("secret"))
		}
		if message.kind == 'E' {
			t.Fatalf("TLS startup error: %q", message.payload)
		}
		if message.kind == 'Z' {
			break
		}
	}
}

func TestSCRAMSHA256AuthenticationWithPasswordAndVerifier(t *testing.T) {
	database, server, address := startTestServer(t, Config{AuthMode: SCRAMSHA256Auth, User: "reuben", Password: "pencil"})
	if server.config.Password != "" {
		t.Fatal("SCRAM server retained plaintext password")
	}
	scramConnect(t, address, "reuben", "pencil", true)
	scramConnect(t, address, "reuben", "wrong-password", false)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	verifier, err := GenerateSCRAMVerifier("pencil", 8192)
	if err != nil {
		t.Fatal(err)
	}
	verifierDatabase, _, verifierAddress := startTestServer(t, Config{AuthMode: SCRAMSHA256Auth, User: "reuben", SCRAMVerifier: verifier})
	defer verifierDatabase.Close()
	scramConnect(t, verifierAddress, "reuben", "pencil", true)
}

func TestSCRAMVerifierValidationAndKnownDerivation(t *testing.T) {
	const verifier = "SCRAM-SHA-256$4096:AAECAwQFBgcICQoLDA0ODw==$zHCdol2044/ZyWzPLi7oxApCkamKw9Z+E4U/QApd/5Y=:dd5peBOitVnLNFu7VmwP+HiDaaw4OUCv396eVCWhYiE="
	credential, err := parseSCRAMVerifier(verifier)
	if err != nil {
		t.Fatal(err)
	}
	storedKey, serverKey := scramKeys("pencil", credential.salt, credential.iterations)
	if subtleCompare(storedKey, credential.storedKey) == false || subtleCompare(serverKey, credential.serverKey) == false {
		t.Fatal("SCRAM SHA-256 derivation does not match known verifier")
	}
	for _, invalid := range []string{
		"", "SCRAM-SHA-1$4096:salt$stored:server",
		"SCRAM-SHA-256$1:AAECAwQFBgcICQoLDA0ODw==$zHCdol2044/ZyWzPLi7oxApCkamKw9Z+E4U/QApd/5Y=:dd5peBOitVnLNFu7VmwP+HiDaaw4OUCv396eVCWhYiE=",
	} {
		if _, err := parseSCRAMVerifier(invalid); err == nil {
			t.Fatalf("invalid verifier accepted: %q", invalid)
		}
	}
}

func scramConnect(t *testing.T, address, user, password string, success bool) {
	t.Helper()
	connection, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	reader, writer := bufio.NewReader(connection), bufio.NewWriter(connection)
	sendStartup(t, writer, user)
	request := receive(t, reader)
	if request.kind != 'R' || len(request.payload) < 6 || binary.BigEndian.Uint32(request.payload[:4]) != 10 || string(request.payload[4:]) != scramMechanism+"\x00\x00" {
		t.Fatalf("SCRAM mechanism request = %q %v", request.kind, request.payload)
	}
	clientNonce := "pebbledb-client-nonce"
	clientFirstBare := "n=,r=" + clientNonce
	clientFirst := "n,," + clientFirstBare
	var initial bytes.Buffer
	initial.Write(cstring(scramMechanism))
	_ = binary.Write(&initial, binary.BigEndian, int32(len(clientFirst)))
	initial.WriteString(clientFirst)
	sendFrontend(t, writer, 'p', initial.Bytes())

	continuation := receive(t, reader)
	if continuation.kind != 'R' || len(continuation.payload) < 5 || binary.BigEndian.Uint32(continuation.payload[:4]) != 11 {
		t.Fatalf("SCRAM continuation = %q %v", continuation.kind, continuation.payload)
	}
	serverFirst := string(continuation.payload[4:])
	attributes, err := parseSCRAMAttributes(serverFirst)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(attributes['r'], clientNonce) {
		t.Fatalf("server nonce %q does not extend client nonce", attributes['r'])
	}
	salt, err := base64.StdEncoding.DecodeString(attributes['s'])
	if err != nil {
		t.Fatal(err)
	}
	iterations, err := strconv.Atoi(attributes['i'])
	if err != nil {
		t.Fatal(err)
	}
	clientFinalWithoutProof := "c=biws,r=" + attributes['r']
	authMessage := clientFirstBare + "," + serverFirst + "," + clientFinalWithoutProof
	saltedPassword := scramPBKDF2([]byte(password), salt, iterations)
	clientKey := scramHMAC(saltedPassword, []byte("Client Key"))
	storedKey := sha256.Sum256(clientKey)
	clientSignature := scramHMAC(storedKey[:], []byte(authMessage))
	proof := make([]byte, sha256.Size)
	for index := range proof {
		proof[index] = clientKey[index] ^ clientSignature[index]
	}
	clientFinal := clientFinalWithoutProof + ",p=" + base64.StdEncoding.EncodeToString(proof)
	sendFrontend(t, writer, 'p', []byte(clientFinal))

	response := receive(t, reader)
	if !success {
		if response.kind != 'E' {
			t.Fatalf("failed SCRAM response = %q, want error", response.kind)
		}
		return
	}
	if response.kind != 'R' || binary.BigEndian.Uint32(response.payload[:4]) != 12 {
		t.Fatalf("SCRAM final = %q %v", response.kind, response.payload)
	}
	expectedServerSignature := base64.StdEncoding.EncodeToString(scramHMAC(scramHMAC(saltedPassword, []byte("Server Key")), []byte(authMessage)))
	if string(response.payload[4:]) != "v="+expectedServerSignature {
		t.Fatalf("server signature = %q, want %q", response.payload[4:], expectedServerSignature)
	}
	if authenticated := receive(t, reader); authenticated.kind != 'R' || binary.BigEndian.Uint32(authenticated.payload) != 0 {
		t.Fatalf("authentication completion = %q %v", authenticated.kind, authenticated.payload)
	}
	for {
		message := receive(t, reader)
		if message.kind == 'E' {
			t.Fatalf("SCRAM startup error: %q", message.payload)
		}
		if message.kind == 'Z' {
			return
		}
	}
}

func subtleCompare(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var difference byte
	for index := range left {
		difference |= left[index] ^ right[index]
	}
	return difference == 0
}

func testTLSCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	encoded, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(encoded)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{encoded}, PrivateKey: privateKey, Leaf: parsed}, roots
}

type startupInfo struct{ backendPID, secret uint32 }
type wireMessage struct {
	kind    byte
	payload []byte
}

func startTestServer(t *testing.T, config Config) (*engine.Engine, *Server, string) {
	t.Helper()
	database, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(database, config)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	return database, server, listener.Addr().String()
}

func connect(t *testing.T, address, user, password string) (net.Conn, *bufio.Reader, *bufio.Writer, startupInfo) {
	t.Helper()
	connection, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := bufio.NewReader(connection), bufio.NewWriter(connection)
	sendStartup(t, writer, user)
	info := startupInfo{}
	for {
		message := receive(t, reader)
		if message.kind == 'R' && binary.BigEndian.Uint32(message.payload) == 3 {
			sendFrontend(t, writer, 'p', cstring(password))
		}
		if message.kind == 'K' {
			info.backendPID = binary.BigEndian.Uint32(message.payload[:4])
			info.secret = binary.BigEndian.Uint32(message.payload[4:])
		}
		if message.kind == 'E' {
			t.Fatalf("startup error: %q", message.payload)
		}
		if message.kind == 'Z' {
			return connection, reader, writer, info
		}
	}
}

func sendStartup(t *testing.T, writer *bufio.Writer, user string) {
	t.Helper()
	var body bytes.Buffer
	_ = binary.Write(&body, binary.BigEndian, int32(protocolVersion3))
	body.Write(cstring("user"))
	body.Write(cstring(user))
	body.Write(cstring("database"))
	body.Write(cstring("pebbledb"))
	body.WriteByte(0)
	_ = binary.Write(writer, binary.BigEndian, int32(body.Len()+4))
	_, _ = writer.Write(body.Bytes())
	_ = writer.Flush()
}

func simple(t *testing.T, reader *bufio.Reader, writer *bufio.Writer, sql string) []wireMessage {
	t.Helper()
	sendFrontend(t, writer, 'Q', cstring(sql))
	var messages []wireMessage
	for {
		message := receive(t, reader)
		messages = append(messages, message)
		if message.kind == 'Z' {
			return messages
		}
	}
}

func sendFrontend(t *testing.T, writer *bufio.Writer, kind byte, payload []byte) {
	t.Helper()
	if err := writeMessage(writer, kind, payload); err != nil {
		t.Fatal(err)
	}
}

func receive(t *testing.T, reader *bufio.Reader) wireMessage {
	t.Helper()
	kind, payload, err := readMessage(reader, defaultMaxMessage)
	if err != nil {
		t.Fatal(err)
	}
	return wireMessage{kind: kind, payload: payload}
}

func decodeTextRow(t *testing.T, payload []byte) []string {
	t.Helper()
	offset := 0
	count, _ := int16Value(payload, &offset)
	result := make([]string, count)
	for index := range result {
		length, _ := int32Value(payload, &offset)
		result[index] = string(payload[offset : offset+int(length)])
		offset += int(length)
	}
	return result
}

func decodeBinaryTextRow(t *testing.T, payload []byte) [][]byte {
	t.Helper()
	offset := 0
	count, _ := int16Value(payload, &offset)
	result := make([][]byte, count)
	for index := range result {
		length, _ := int32Value(payload, &offset)
		result[index] = append([]byte(nil), payload[offset:offset+int(length)]...)
		offset += int(length)
	}
	return result
}

func countType(messages []wireMessage, kind byte) int { return len(messagesOfType(messages, kind)) }
func messagesOfType(messages []wireMessage, kind byte) []wireMessage {
	var result []wireMessage
	for _, message := range messages {
		if message.kind == kind {
			result = append(result, message)
		}
	}
	return result
}
func messageKinds(messages []wireMessage) []byte {
	result := make([]byte, len(messages))
	for index := range messages {
		result[index] = messages[index].kind
	}
	return result
}
