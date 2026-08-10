package pgwire

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

const (
	protocolVersion3  = 196608
	sslRequestCode    = 80877103
	cancelRequestCode = 80877102
	defaultMaxMessage = 16 << 20
)

func readStartup(reader *bufio.Reader, maximum int) (int32, []byte, error) {
	var length int32
	if err := binary.Read(reader, binary.BigEndian, &length); err != nil {
		return 0, nil, err
	}
	if length < 8 || int(length) > maximum {
		return 0, nil, fmt.Errorf("pgwire: invalid startup message length %d", length)
	}
	payload := make([]byte, int(length)-4)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	return int32(binary.BigEndian.Uint32(payload[:4])), payload[4:], nil
}

func readMessage(reader *bufio.Reader, maximum int) (byte, []byte, error) {
	typeByte, err := reader.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	var length int32
	if err := binary.Read(reader, binary.BigEndian, &length); err != nil {
		return 0, nil, err
	}
	if length < 4 || int(length) > maximum {
		return 0, nil, fmt.Errorf("pgwire: invalid message length %d", length)
	}
	payload := make([]byte, int(length)-4)
	_, err = io.ReadFull(reader, payload)
	return typeByte, payload, err
}

func writeMessage(writer *bufio.Writer, typeByte byte, payload []byte) error {
	if err := writer.WriteByte(typeByte); err != nil {
		return err
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(payload)+4))
	if _, err := writer.Write(length[:]); err != nil {
		return err
	}
	if _, err := writer.Write(payload); err != nil {
		return err
	}
	return writer.Flush()
}

func authentication(code int32) []byte {
	var payload [4]byte
	binary.BigEndian.PutUint32(payload[:], uint32(code))
	return payload[:]
}

func cstring(value string) []byte { return append([]byte(value), 0) }

func readCString(payload []byte, offset *int) (string, error) {
	if *offset >= len(payload) {
		return "", fmt.Errorf("pgwire: missing string")
	}
	end := bytes.IndexByte(payload[*offset:], 0)
	if end < 0 {
		return "", fmt.Errorf("pgwire: unterminated string")
	}
	value := string(payload[*offset : *offset+end])
	*offset += end + 1
	return value, nil
}

func parseStartupParameters(payload []byte) (map[string]string, error) {
	parameters := make(map[string]string)
	offset := 0
	for offset < len(payload) && payload[offset] != 0 {
		key, err := readCString(payload, &offset)
		if err != nil {
			return nil, err
		}
		value, err := readCString(payload, &offset)
		if err != nil {
			return nil, err
		}
		parameters[key] = value
	}
	return parameters, nil
}

func int16Value(payload []byte, offset *int) (int16, error) {
	if *offset+2 > len(payload) {
		return 0, io.ErrUnexpectedEOF
	}
	value := int16(binary.BigEndian.Uint16(payload[*offset : *offset+2]))
	*offset += 2
	return value, nil
}

func int32Value(payload []byte, offset *int) (int32, error) {
	if *offset+4 > len(payload) {
		return 0, io.ErrUnexpectedEOF
	}
	value := int32(binary.BigEndian.Uint32(payload[*offset : *offset+4]))
	*offset += 4
	return value, nil
}
