package pgwire

import (
	"bufio"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	scramMechanism         = "SCRAM-SHA-256"
	defaultSCRAMIterations = 4096
	minimumSCRAMIterations = 4096
	maximumSCRAMIterations = 1_000_000
)

type scramCredential struct {
	iterations int
	salt       []byte
	storedKey  []byte
	serverKey  []byte
}

// GenerateSCRAMVerifier creates a PostgreSQL-compatible SCRAM-SHA-256 verifier
// suitable for Config.SCRAMVerifier. Zero iterations selects PostgreSQL's
// interoperable default of 4096.
func GenerateSCRAMVerifier(password string, iterations int) (string, error) {
	if iterations == 0 {
		iterations = defaultSCRAMIterations
	}
	credential, err := newSCRAMCredential(password, iterations)
	if err != nil {
		return "", err
	}
	return credential.verifier(), nil
}

func newSCRAMCredential(password string, iterations int) (*scramCredential, error) {
	if password == "" || !utf8.ValidString(password) {
		return nil, fmt.Errorf("pgwire: SCRAM password must be non-empty UTF-8")
	}
	if iterations < minimumSCRAMIterations || iterations > maximumSCRAMIterations {
		return nil, fmt.Errorf("pgwire: SCRAM iteration count must be between %d and %d", minimumSCRAMIterations, maximumSCRAMIterations)
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("pgwire: generate SCRAM salt: %w", err)
	}
	storedKey, serverKey := scramKeys(password, salt, iterations)
	return &scramCredential{iterations: iterations, salt: salt, storedKey: storedKey, serverKey: serverKey}, nil
}

func parseSCRAMVerifier(verifier string) (*scramCredential, error) {
	const prefix = scramMechanism + "$"
	if !strings.HasPrefix(verifier, prefix) {
		return nil, fmt.Errorf("pgwire: invalid SCRAM verifier mechanism")
	}
	parts := strings.Split(strings.TrimPrefix(verifier, prefix), "$")
	if len(parts) != 2 {
		return nil, fmt.Errorf("pgwire: invalid SCRAM verifier fields")
	}
	iterationAndSalt := strings.Split(parts[0], ":")
	keys := strings.Split(parts[1], ":")
	if len(iterationAndSalt) != 2 || len(keys) != 2 {
		return nil, fmt.Errorf("pgwire: invalid SCRAM verifier fields")
	}
	iterations, err := strconv.Atoi(iterationAndSalt[0])
	if err != nil || iterations < minimumSCRAMIterations || iterations > maximumSCRAMIterations {
		return nil, fmt.Errorf("pgwire: invalid SCRAM verifier iteration count")
	}
	decode := func(value, name string) ([]byte, error) {
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return nil, fmt.Errorf("pgwire: invalid SCRAM verifier %s: %w", name, err)
		}
		return decoded, nil
	}
	salt, err := decode(iterationAndSalt[1], "salt")
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return nil, fmt.Errorf("pgwire: invalid SCRAM verifier salt")
	}
	storedKey, err := decode(keys[0], "stored key")
	if err != nil || len(storedKey) != sha256.Size {
		return nil, fmt.Errorf("pgwire: invalid SCRAM verifier stored key")
	}
	serverKey, err := decode(keys[1], "server key")
	if err != nil || len(serverKey) != sha256.Size {
		return nil, fmt.Errorf("pgwire: invalid SCRAM verifier server key")
	}
	return &scramCredential{iterations: iterations, salt: salt, storedKey: storedKey, serverKey: serverKey}, nil
}

func (credential *scramCredential) verifier() string {
	return fmt.Sprintf("%s$%d:%s$%s:%s", scramMechanism, credential.iterations,
		base64.StdEncoding.EncodeToString(credential.salt),
		base64.StdEncoding.EncodeToString(credential.storedKey),
		base64.StdEncoding.EncodeToString(credential.serverKey))
}

func (server *Server) authenticateSCRAM(reader *bufio.Reader, writer *bufio.Writer) error {
	request := append(authentication(10), []byte(scramMechanism)...)
	request = append(request, 0, 0)
	if err := writeMessage(writer, 'R', request); err != nil {
		return err
	}
	typeByte, payload, err := readMessage(reader, server.config.MaxMessageBytes)
	if err != nil {
		return err
	}
	if typeByte != 'p' {
		return fmt.Errorf("expected SASL initial response")
	}
	clientFirst, err := parseSASLInitial(payload)
	if err != nil {
		return err
	}
	gs2Header, clientFirstBare, clientNonce, err := parseClientFirst(clientFirst)
	if err != nil {
		return err
	}
	serverNonce, err := randomSCRAMNonce()
	if err != nil {
		return err
	}
	combinedNonce := clientNonce + serverNonce
	serverFirst := fmt.Sprintf("r=%s,s=%s,i=%d", combinedNonce,
		base64.StdEncoding.EncodeToString(server.scram.salt), server.scram.iterations)
	continuation := append(authentication(11), []byte(serverFirst)...)
	if err := writeMessage(writer, 'R', continuation); err != nil {
		return err
	}
	typeByte, clientFinalPayload, err := readMessage(reader, server.config.MaxMessageBytes)
	if err != nil {
		return err
	}
	if typeByte != 'p' {
		return fmt.Errorf("expected SASL response")
	}
	clientFinalWithoutProof, proof, err := parseClientFinal(string(clientFinalPayload), gs2Header, combinedNonce)
	if err != nil {
		return err
	}
	authMessage := clientFirstBare + "," + serverFirst + "," + clientFinalWithoutProof
	clientSignature := scramHMAC(server.scram.storedKey, []byte(authMessage))
	if len(proof) != sha256.Size {
		return fmt.Errorf("SCRAM authentication failed")
	}
	clientKey := make([]byte, sha256.Size)
	for index := range clientKey {
		clientKey[index] = proof[index] ^ clientSignature[index]
	}
	computedStoredKey := sha256.Sum256(clientKey)
	if subtle.ConstantTimeCompare(computedStoredKey[:], server.scram.storedKey) != 1 {
		return fmt.Errorf("SCRAM authentication failed")
	}
	serverSignature := scramHMAC(server.scram.serverKey, []byte(authMessage))
	final := append(authentication(12), []byte("v="+base64.StdEncoding.EncodeToString(serverSignature))...)
	if err := writeMessage(writer, 'R', final); err != nil {
		return err
	}
	return writeMessage(writer, 'R', authentication(0))
}

func parseSASLInitial(payload []byte) (string, error) {
	offset := 0
	mechanism, err := readCString(payload, &offset)
	if err != nil || mechanism != scramMechanism || offset+4 > len(payload) {
		return "", fmt.Errorf("pgwire: invalid SCRAM mechanism selection")
	}
	length := int(int32(binary.BigEndian.Uint32(payload[offset : offset+4])))
	offset += 4
	if length < 0 || length != len(payload)-offset {
		return "", fmt.Errorf("pgwire: invalid SCRAM initial response length")
	}
	return string(payload[offset:]), nil
}

func parseClientFirst(message string) (gs2Header, bare, nonce string, err error) {
	if strings.HasPrefix(message, "n,,") || strings.HasPrefix(message, "y,,") {
		gs2Header, bare = message[:3], message[3:]
	} else {
		return "", "", "", fmt.Errorf("pgwire: unsupported SCRAM channel binding")
	}
	attributes, err := parseSCRAMAttributes(bare)
	if err != nil {
		return "", "", "", err
	}
	if _, ok := attributes['n']; !ok {
		return "", "", "", fmt.Errorf("pgwire: SCRAM client username is missing")
	}
	nonce = attributes['r']
	if !validSCRAMNonce(nonce) {
		return "", "", "", fmt.Errorf("pgwire: invalid SCRAM client nonce")
	}
	return gs2Header, bare, nonce, nil
}

func parseClientFinal(message, gs2Header, nonce string) (string, []byte, error) {
	proofOffset := strings.LastIndex(message, ",p=")
	if proofOffset < 0 || strings.Contains(message[proofOffset+3:], ",") {
		return "", nil, fmt.Errorf("pgwire: SCRAM client proof is missing or misplaced")
	}
	withoutProof := message[:proofOffset]
	attributes, err := parseSCRAMAttributes(withoutProof)
	if err != nil {
		return "", nil, err
	}
	channelBinding, err := base64.StdEncoding.DecodeString(attributes['c'])
	if err != nil || subtle.ConstantTimeCompare(channelBinding, []byte(gs2Header)) != 1 {
		return "", nil, fmt.Errorf("pgwire: SCRAM channel binding does not match")
	}
	if attributes['r'] != nonce {
		return "", nil, fmt.Errorf("pgwire: SCRAM nonce does not match")
	}
	proof, err := base64.StdEncoding.DecodeString(message[proofOffset+3:])
	if err != nil {
		return "", nil, fmt.Errorf("pgwire: invalid SCRAM client proof")
	}
	return withoutProof, proof, nil
}

func parseSCRAMAttributes(message string) (map[byte]string, error) {
	attributes := make(map[byte]string)
	for _, field := range strings.Split(message, ",") {
		if len(field) < 2 || field[1] != '=' {
			return nil, fmt.Errorf("pgwire: malformed SCRAM attribute")
		}
		name := field[0]
		if name == 'm' {
			return nil, fmt.Errorf("pgwire: unsupported mandatory SCRAM extension")
		}
		if _, duplicate := attributes[name]; duplicate {
			return nil, fmt.Errorf("pgwire: duplicate SCRAM attribute")
		}
		attributes[name] = field[2:]
	}
	return attributes, nil
}

func validSCRAMNonce(nonce string) bool {
	if len(nonce) < 8 || len(nonce) > 1024 {
		return false
	}
	for _, current := range []byte(nonce) {
		if current < 0x21 || current > 0x7e || current == ',' {
			return false
		}
	}
	return true
}

func randomSCRAMNonce() (string, error) {
	value := make([]byte, 18)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("pgwire: generate SCRAM nonce: %w", err)
	}
	return base64.RawStdEncoding.EncodeToString(value), nil
}

func scramKeys(password string, salt []byte, iterations int) ([]byte, []byte) {
	saltedPassword := scramPBKDF2([]byte(password), salt, iterations)
	clientKey := scramHMAC(saltedPassword, []byte("Client Key"))
	storedKey := sha256.Sum256(clientKey)
	return storedKey[:], scramHMAC(saltedPassword, []byte("Server Key"))
}

func scramPBKDF2(password, salt []byte, iterations int) []byte {
	block := append(append([]byte(nil), salt...), 0, 0, 0, 1)
	current := scramHMAC(password, block)
	result := append([]byte(nil), current...)
	for iteration := 1; iteration < iterations; iteration++ {
		current = scramHMAC(password, current)
		for index := range result {
			result[index] ^= current[index]
		}
	}
	return result
}

func scramHMAC(key, message []byte) []byte {
	digest := hmac.New(sha256.New, key)
	_, _ = digest.Write(message)
	return digest.Sum(nil)
}
