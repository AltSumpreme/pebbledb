package db

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

func SerializeRow(row Row, columns []Column) ([]byte, error) {
	var buf bytes.Buffer
	for _, col := range columns {
		val := row.Value[col.Name]
		switch col.Type {
		case TypeInt:
			if v, ok := val.(int); ok {
				if err := binary.Write(&buf, binary.LittleEndian, int32(v)); err != nil {
					return nil, err
				}

			}
		case TypeString:
			if str, ok := val.(string); ok {
				if err := binary.Write(&buf, binary.LittleEndian, uint16(len(str))); err != nil {
					return nil, err
				}
				if _, err := buf.Write([]byte(str)); err != nil {
					return nil, err
				}
			}

		}
	}
	return buf.Bytes(), nil
}

func DeserializeRow(data []byte, columns []Column) (Row, error) {
	rowValue := make(map[string]interface{}, len(columns))
	buf := bytes.NewBuffer(data)

	for _, col := range columns {
		switch col.Type {
		case TypeInt:
			var val int32
			if err := binary.Read(buf, binary.LittleEndian, &val); err != nil {
				return Row{}, err
			}
			rowValue[col.Name] = int(val)
		case TypeString:
			var length uint16
			if err := binary.Read(buf, binary.LittleEndian, &length); err != nil {
				return Row{}, err
			}
			strData := make([]byte, length)
			if _, err := io.ReadFull(buf, strData); err != nil {
				return Row{}, err
			}
			rowValue[col.Name] = string(strData)
		}
	}
	fmt.Printf("%v\n", rowValue)
	return Row{
		Value: rowValue,
	}, nil
}
