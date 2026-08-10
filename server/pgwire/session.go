package pgwire

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"pebbledb/sql/executor"
)

type result struct{ value executor.Result }
type column struct{ value executor.ResultColumn }

type preparedStatement struct {
	sql       string
	paramOIDs []uint32
}

type portal struct {
	sql           string
	columns       []executor.ResultColumn
	resultFormats []int16
	results       []executor.Result
	resultIndex   int
	rowOffset     int
	executed      bool
}

type client struct {
	server     *Server
	connection net.Conn
	reader     *bufio.Reader
	writer     *bufio.Writer
	pid        uint32
	secret     uint32
	parameters map[string]string
	prepared   map[string]preparedStatement
	portals    map[string]*portal

	cancelMu sync.Mutex
	cancel   context.CancelFunc

	inTransaction     bool
	failedTransaction bool
	skipUntilSync     bool
}

func (client *client) startupComplete() error {
	parameters := [][2]string{
		{"server_version", client.server.config.ServerVersion},
		{"server_encoding", "UTF8"},
		{"client_encoding", "UTF8"},
		{"standard_conforming_strings", "on"},
		{"integer_datetimes", "on"},
	}
	for _, parameter := range parameters {
		payload := append(cstring(parameter[0]), cstring(parameter[1])...)
		if err := writeMessage(client.writer, 'S', payload); err != nil {
			return err
		}
	}
	var backend [8]byte
	binary.BigEndian.PutUint32(backend[:4], client.pid)
	binary.BigEndian.PutUint32(backend[4:], client.secret)
	if err := writeMessage(client.writer, 'K', backend[:]); err != nil {
		return err
	}
	return client.ready()
}

func (client *client) loop() {
	for {
		typeByte, payload, err := readMessage(client.reader, client.server.config.MaxMessageBytes)
		if err != nil {
			return
		}
		if client.skipUntilSync && typeByte != 'S' && typeByte != 'X' {
			continue
		}
		if err := client.handle(typeByte, payload); err != nil {
			if isDisconnect(err) {
				return
			}
			_ = writeError(client.writer, sqlState(err), err)
			if typeByte == 'Q' {
				_ = client.ready()
			} else {
				client.skipUntilSync = true
			}
		}
		if typeByte == 'X' {
			return
		}
	}
}

func (client *client) handle(typeByte byte, payload []byte) error {
	switch typeByte {
	case 'Q':
		return client.simpleQuery(strings.TrimSuffix(string(payload), "\x00"))
	case 'P':
		return client.parse(payload)
	case 'B':
		return client.bind(payload)
	case 'D':
		return client.describe(payload)
	case 'E':
		return client.executePortal(payload)
	case 'C':
		return client.closeObject(payload)
	case 'S':
		client.skipUntilSync = false
		return client.ready()
	case 'H':
		return client.writer.Flush()
	case 'X':
		return nil
	default:
		return fmt.Errorf("unsupported frontend message %q", typeByte)
	}
}

func (client *client) simpleQuery(sql string) error {
	ctx, cancel := context.WithCancel(context.Background())
	client.setCancel(cancel)
	defer client.clearCancel(cancel)
	results, err := client.server.execute(client, ctx, sql)
	for _, current := range results {
		if writeErr := client.writeResult(current.value, nil, 0, nil); writeErr != nil {
			return writeErr
		}
	}
	if err != nil {
		return err
	}
	if len(results) == 0 {
		if err := writeMessage(client.writer, 'I', nil); err != nil {
			return err
		}
	}
	return client.ready()
}

func (client *client) parse(payload []byte) error {
	offset := 0
	name, err := readCString(payload, &offset)
	if err != nil {
		return err
	}
	sql, err := readCString(payload, &offset)
	if err != nil {
		return err
	}
	count, err := int16Value(payload, &offset)
	if err != nil || count < 0 {
		return fmt.Errorf("invalid parameter type count")
	}
	oids := make([]uint32, count)
	for index := range oids {
		value, err := int32Value(payload, &offset)
		if err != nil {
			return err
		}
		oids[index] = uint32(value)
	}
	if offset != len(payload) {
		return fmt.Errorf("trailing Parse data")
	}
	client.prepared[name] = preparedStatement{sql: sql, paramOIDs: oids}
	return writeMessage(client.writer, '1', nil)
}

func (client *client) bind(payload []byte) error {
	offset := 0
	portalName, err := readCString(payload, &offset)
	if err != nil {
		return err
	}
	statementName, err := readCString(payload, &offset)
	if err != nil {
		return err
	}
	statement, exists := client.prepared[statementName]
	if !exists {
		return fmt.Errorf("prepared statement %q does not exist", statementName)
	}
	formatCount, err := int16Value(payload, &offset)
	if err != nil || formatCount < 0 {
		return fmt.Errorf("invalid parameter format count")
	}
	parameterFormats := make([]int16, formatCount)
	for index := range parameterFormats {
		parameterFormats[index], err = int16Value(payload, &offset)
		if err != nil {
			return err
		}
	}
	parameterCount, err := int16Value(payload, &offset)
	if err != nil || parameterCount < 0 {
		return fmt.Errorf("invalid parameter count")
	}
	formats, err := normalizeFormats(parameterFormats, int(parameterCount))
	if err != nil {
		return err
	}
	parameters := make([][]byte, parameterCount)
	nulls := make([]bool, parameterCount)
	for index := range parameters {
		length, err := int32Value(payload, &offset)
		if err != nil {
			return err
		}
		if length == -1 {
			nulls[index] = true
			continue
		}
		if length < 0 || int64(offset)+int64(length) > int64(len(payload)) {
			return fmt.Errorf("invalid parameter length")
		}
		parameters[index] = append([]byte(nil), payload[offset:offset+int(length)]...)
		offset += int(length)
	}
	resultFormatCount, err := int16Value(payload, &offset)
	if err != nil || resultFormatCount < 0 {
		return fmt.Errorf("invalid result format count")
	}
	resultFormats := make([]int16, resultFormatCount)
	for index := range resultFormats {
		resultFormats[index], err = int16Value(payload, &offset)
		if err != nil {
			return err
		}
	}
	if offset != len(payload) {
		return fmt.Errorf("trailing Bind data")
	}
	boundSQL, err := substituteParameters(statement.sql, statement.paramOIDs, formats, parameters, nulls)
	if err != nil {
		return err
	}
	columns, err := client.server.describe(client, boundSQL)
	if err != nil {
		return err
	}
	executorColumns := make([]executor.ResultColumn, len(columns))
	for index := range columns {
		executorColumns[index] = columns[index].value
	}
	normalizedResults, err := normalizeFormats(resultFormats, len(executorColumns))
	if err != nil {
		return err
	}
	client.portals[portalName] = &portal{sql: boundSQL, columns: executorColumns, resultFormats: normalizedResults}
	return writeMessage(client.writer, '2', nil)
}

func (client *client) describe(payload []byte) error {
	if len(payload) < 2 {
		return fmt.Errorf("invalid Describe message")
	}
	kind, offset := payload[0], 1
	name, err := readCString(payload, &offset)
	if err != nil {
		return err
	}
	switch kind {
	case 'S':
		statement, exists := client.prepared[name]
		if !exists {
			return fmt.Errorf("prepared statement %q does not exist", name)
		}
		var parameters bytes.Buffer
		_ = binary.Write(&parameters, binary.BigEndian, int16(len(statement.paramOIDs)))
		for _, oid := range statement.paramOIDs {
			_ = binary.Write(&parameters, binary.BigEndian, oid)
		}
		if err := writeMessage(client.writer, 't', parameters.Bytes()); err != nil {
			return err
		}
		prototype := prototypeSQL(statement.sql, statement.paramOIDs)
		columns, err := client.server.describe(client, prototype)
		if err != nil {
			return err
		}
		if len(columns) == 0 {
			return writeMessage(client.writer, 'n', nil)
		}
		values := make([]executor.ResultColumn, len(columns))
		for index := range columns {
			values[index] = columns[index].value
		}
		return writeMessage(client.writer, 'T', rowDescription(values, nil))
	case 'P':
		portal, exists := client.portals[name]
		if !exists {
			return fmt.Errorf("portal %q does not exist", name)
		}
		if len(portal.columns) == 0 {
			return writeMessage(client.writer, 'n', nil)
		}
		return writeMessage(client.writer, 'T', rowDescription(portal.columns, portal.resultFormats))
	default:
		return fmt.Errorf("invalid Describe target %q", kind)
	}
}

func (client *client) executePortal(payload []byte) error {
	offset := 0
	name, err := readCString(payload, &offset)
	if err != nil {
		return err
	}
	maximum, err := int32Value(payload, &offset)
	if err != nil || maximum < 0 {
		return fmt.Errorf("invalid Execute row limit")
	}
	portal, exists := client.portals[name]
	if !exists {
		return fmt.Errorf("portal %q does not exist", name)
	}
	if !portal.executed {
		ctx, cancel := context.WithCancel(context.Background())
		client.setCancel(cancel)
		results, executeErr := client.server.execute(client, ctx, portal.sql)
		client.clearCancel(cancel)
		portal.executed = true
		portal.results = make([]executor.Result, len(results))
		for index := range results {
			portal.results[index] = results[index].value
		}
		if executeErr != nil {
			return executeErr
		}
	}
	remaining := int(maximum)
	if maximum == 0 {
		remaining = int(^uint(0) >> 1)
	}
	for portal.resultIndex < len(portal.results) {
		current := portal.results[portal.resultIndex]
		for portal.rowOffset < len(current.Rows) && remaining > 0 {
			encoded, err := dataRow(current.Rows[portal.rowOffset], portal.resultFormats)
			if err != nil {
				return err
			}
			if err := writeMessage(client.writer, 'D', encoded); err != nil {
				return err
			}
			portal.rowOffset++
			remaining--
		}
		if portal.rowOffset < len(current.Rows) {
			return writeMessage(client.writer, 's', nil)
		}
		if err := writeMessage(client.writer, 'C', cstring(commandTag(current))); err != nil {
			return err
		}
		portal.resultIndex++
		portal.rowOffset = 0
	}
	return nil
}

func (client *client) closeObject(payload []byte) error {
	if len(payload) < 2 {
		return fmt.Errorf("invalid Close message")
	}
	kind, offset := payload[0], 1
	name, err := readCString(payload, &offset)
	if err != nil {
		return err
	}
	switch kind {
	case 'S':
		delete(client.prepared, name)
	case 'P':
		delete(client.portals, name)
	default:
		return fmt.Errorf("invalid Close target")
	}
	return writeMessage(client.writer, '3', nil)
}

func (client *client) writeResult(result executor.Result, formats []int16, maximum int, offset *int) error {
	if len(result.Columns) > 0 {
		normalized, err := normalizeFormats(formats, len(result.Columns))
		if err != nil {
			return err
		}
		if err := writeMessage(client.writer, 'T', rowDescription(result.Columns, normalized)); err != nil {
			return err
		}
		for _, row := range result.Rows {
			encoded, err := dataRow(row, normalized)
			if err != nil {
				return err
			}
			if err := writeMessage(client.writer, 'D', encoded); err != nil {
				return err
			}
		}
	}
	return writeMessage(client.writer, 'C', cstring(commandTag(result)))
}

func (client *client) ready() error {
	status := byte('I')
	if client.failedTransaction {
		status = 'E'
	} else if client.inTransaction {
		status = 'T'
	}
	return writeMessage(client.writer, 'Z', []byte{status})
}

func (client *client) setCancel(cancel context.CancelFunc) {
	client.cancelMu.Lock()
	client.cancel = cancel
	client.cancelMu.Unlock()
}

func (client *client) clearCancel(cancel context.CancelFunc) {
	client.cancelMu.Lock()
	client.cancel = nil
	client.cancelMu.Unlock()
	cancel()
}

func (client *client) cancelQuery() {
	client.cancelMu.Lock()
	if client.cancel != nil {
		client.cancel()
	}
	client.cancelMu.Unlock()
}

func commandTag(result executor.Result) string {
	switch result.Message {
	case "SELECT":
		return "SELECT " + strconv.Itoa(len(result.Rows))
	case "INSERT":
		return fmt.Sprintf("INSERT 0 %d", result.RowsAffected)
	case "UPDATE", "DELETE":
		return fmt.Sprintf("%s %d", result.Message, result.RowsAffected)
	default:
		return result.Message
	}
}

func sqlState(err error) string {
	if errors.Is(err, context.Canceled) {
		return "57014"
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "duplicate"), strings.Contains(message, "unique"):
		return "23505"
	case strings.Contains(message, "not found"), strings.Contains(message, "does not exist"):
		return "42P01"
	case strings.Contains(message, "syntax"), strings.Contains(message, "expected"):
		return "42601"
	case strings.Contains(message, "transaction"):
		return "25P02"
	default:
		return "XX000"
	}
}
