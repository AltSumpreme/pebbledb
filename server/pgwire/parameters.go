package pgwire

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

func substituteParameters(sql string, oids []uint32, formats []int16, values [][]byte, nulls []bool) (string, error) {
	if len(values) != len(formats) || len(values) != len(nulls) {
		return "", fmt.Errorf("pgwire: inconsistent parameter metadata")
	}
	literals := make([]string, len(values))
	for index := range values {
		if nulls[index] {
			literals[index] = "NULL"
			continue
		}
		oid := uint32(0)
		if index < len(oids) {
			oid = oids[index]
		}
		literal, err := parameterLiteral(oid, formats[index], values[index])
		if err != nil {
			return "", fmt.Errorf("pgwire: parameter $%d: %w", index+1, err)
		}
		literals[index] = literal
	}
	return replacePlaceholders(sql, literals)
}

func parameterLiteral(oid uint32, format int16, value []byte) (string, error) {
	if format == 1 {
		switch oid {
		case boolOID:
			if len(value) != 1 || value[0] > 1 {
				return "", fmt.Errorf("invalid binary BOOL")
			}
			if value[0] == 1 {
				return "TRUE", nil
			}
			return "FALSE", nil
		case int4OID:
			if len(value) != 4 {
				return "", fmt.Errorf("invalid binary INT4")
			}
			return strconv.FormatInt(int64(int32(binary.BigEndian.Uint32(value))), 10), nil
		case int8OID:
			if len(value) != 8 {
				return "", fmt.Errorf("invalid binary INT8")
			}
			return strconv.FormatInt(int64(binary.BigEndian.Uint64(value)), 10), nil
		case textOID, 0:
			return quoteSQL(string(value)), nil
		default:
			return "", fmt.Errorf("binary parameter OID %d is not supported", oid)
		}
	}
	if format != 0 {
		return "", fmt.Errorf("unsupported parameter format %d", format)
	}
	raw := string(value)
	switch oid {
	case boolOID:
		switch strings.ToLower(raw) {
		case "t", "true", "1":
			return "TRUE", nil
		case "f", "false", "0":
			return "FALSE", nil
		default:
			return "", fmt.Errorf("invalid BOOL")
		}
	case int4OID:
		if _, err := strconv.ParseInt(raw, 10, 32); err != nil {
			return "", fmt.Errorf("invalid INT4")
		}
		return raw, nil
	case int8OID:
		if _, err := strconv.ParseInt(raw, 10, 64); err != nil {
			return "", fmt.Errorf("invalid INT8")
		}
		return raw, nil
	case numericOID:
		if !isDecimal(raw) {
			return "", fmt.Errorf("invalid NUMERIC")
		}
		return raw, nil
	case textOID:
		return quoteSQL(raw), nil
	case 0:
		if _, err := strconv.ParseInt(raw, 10, 64); err == nil || isDecimal(raw) {
			return raw, nil
		}
		if strings.EqualFold(raw, "true") || strings.EqualFold(raw, "false") {
			return strings.ToUpper(raw), nil
		}
		return quoteSQL(raw), nil
	default:
		return quoteSQL(raw), nil
	}
}

func replacePlaceholders(sql string, literals []string) (string, error) {
	var result strings.Builder
	inSingle, inDouble, lineComment, blockComment := false, false, false, false
	for index := 0; index < len(sql); {
		if lineComment {
			result.WriteByte(sql[index])
			if sql[index] == '\n' {
				lineComment = false
			}
			index++
			continue
		}
		if blockComment {
			if index+1 < len(sql) && sql[index:index+2] == "*/" {
				result.WriteString("*/")
				index += 2
				blockComment = false
			} else {
				result.WriteByte(sql[index])
				index++
			}
			continue
		}
		if !inSingle && !inDouble && index+1 < len(sql) {
			if sql[index:index+2] == "--" {
				lineComment = true
				result.WriteString("--")
				index += 2
				continue
			}
			if sql[index:index+2] == "/*" {
				blockComment = true
				result.WriteString("/*")
				index += 2
				continue
			}
		}
		if sql[index] == '\'' && !inDouble {
			result.WriteByte(sql[index])
			if inSingle && index+1 < len(sql) && sql[index+1] == '\'' {
				result.WriteByte(sql[index+1])
				index += 2
				continue
			}
			inSingle = !inSingle
			index++
			continue
		}
		if sql[index] == '"' && !inSingle {
			inDouble = !inDouble
			result.WriteByte(sql[index])
			index++
			continue
		}
		if sql[index] == '$' && !inSingle && !inDouble {
			end := index + 1
			for end < len(sql) && unicode.IsDigit(rune(sql[end])) {
				end++
			}
			if end > index+1 {
				position, _ := strconv.Atoi(sql[index+1 : end])
				if position < 1 || position > len(literals) {
					return "", fmt.Errorf("pgwire: no value for parameter $%d", position)
				}
				result.WriteString(literals[position-1])
				index = end
				continue
			}
		}
		result.WriteByte(sql[index])
		index++
	}
	return result.String(), nil
}

func prototypeSQL(sql string, oids []uint32) string {
	literals := make([]string, len(oids))
	for index, oid := range oids {
		switch oid {
		case boolOID:
			literals[index] = "FALSE"
		case int4OID, int8OID, numericOID:
			literals[index] = "0"
		default:
			literals[index] = "''"
		}
	}
	result, err := replacePlaceholders(sql, literals)
	if err != nil {
		return sql
	}
	return result
}

func quoteSQL(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

func isDecimal(value string) bool {
	if value == "" {
		return false
	}
	if value[0] == '+' || value[0] == '-' {
		value = value[1:]
	}
	dot, digits := false, 0
	for _, current := range value {
		if current == '.' && !dot {
			dot = true
			continue
		}
		if current < '0' || current > '9' {
			return false
		}
		digits++
	}
	return digits > 0
}
