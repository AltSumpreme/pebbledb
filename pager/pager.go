package pager

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"pebbledb/db"
)

const (
	PageSize         = 4096
	PageHeaderSize   = 16
	ItemIDSize       = 6
	MaxItemsPerPage  = 128
	SpecialSpaceSize = 0 // Reserved for future user for indexing metadata
	DataRegionSize   = PageSize - PageHeaderSize - MaxItemsPerPage*ItemIDSize - SpecialSpaceSize
)

type PageHeader struct {
	LSN      uint64 // Future WAL support
	NumItems uint16
	PdLower  uint16
	PdUpper  uint16
}

type ItemID struct {
	Offset      uint16
	Length      uint16
	DeletedFlag uint8
	_           [1]byte // Padding to align to 6 bytes
}

type Page struct {
	Header PageHeader
	Items  [MaxItemsPerPage]ItemID
	Data   [DataRegionSize]byte
}

func NewPage() *Page {
	return &Page{
		Header: PageHeader{
			PdLower: PageHeaderSize,
			PdUpper: DataRegionSize,
		},
		Items: [MaxItemsPerPage]ItemID{},
		Data:  [DataRegionSize]byte{},
	}
}

func (p *Page) InsertTuple(record []byte) (int, error) {
	if len(record) > DataRegionSize {
		return -1, errors.New("record too large")
	}
	if len(record) == 0 {
		return -1, errors.New("record cannot be empty")
	}
	if p.Header.NumItems >= MaxItemsPerPage {
		return -1, errors.New("page is full")
	}

	// 🧠 Calculate available free space
	freeSpace := int(p.Header.PdUpper) - int(p.Header.PdLower)
	requiredSpace := len(record) + ItemIDSize
	if requiredSpace > freeSpace {
		return -1, errors.New("not enough space in page")
	}

	// 👇 Insert the record at the top
	newUpper := p.Header.PdUpper - uint16(len(record))
	copy(p.Data[newUpper:], record)

	// 👇 Add the metadata for this tuple
	slot := int(p.Header.NumItems)
	p.Items[slot] = ItemID{
		Offset:      newUpper,
		Length:      uint16(len(record)),
		DeletedFlag: 1,
	}

	p.Header.PdUpper = newUpper
	p.Header.PdLower += ItemIDSize
	p.Header.NumItems++

	return slot, nil
}

func (p *Page) ReadTuple(slot int) ([]byte, error) {

	if slot < 0 || slot >= int(p.Header.NumItems) {
		return nil, errors.New("invalid slot number")
	}
	items := p.Items[slot]
	if items.DeletedFlag == 0 {
		return nil, errors.New("tuple has been deleted")
	}
	data := make([]byte, items.Length)
	copy(data, p.Data[items.Offset:items.Offset+items.Length])
	return data, nil
}

func (p *Page) DeleteTuple(slot int) error {
	if slot < 0 || slot >= int(p.Header.NumItems) {
		return errors.New("invalid slot number")
	}
	items := p.Items[slot]
	if items.DeletedFlag == 0 {
		return errors.New("tuple has already been deleted")
	}
	p.Items[slot].DeletedFlag = 0
	return nil
}
func SerializeRow(row db.Row, columns []db.Column) ([]byte, error) {
	var buf bytes.Buffer
	for _, col := range columns {
		val := row[col.Name]
		switch col.Type {
		case db.TypeInt:
			if v, ok := val.(int); ok {
				if err := binary.Write(&buf, binary.LittleEndian, int32(v)); err != nil {
					return nil, err
				}

			}
		case db.TypeString:
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

func DeserializeRow(data []byte, columns []db.Column) (db.Row, error) {
	row := make(db.Row)
	buf := bytes.NewBuffer(data)

	for _, col := range columns {
		switch col.Type {
		case db.TypeInt:
			var val int32
			if err := binary.Read(buf, binary.LittleEndian, &val); err != nil {
				return nil, err
			}
			row[col.Name] = int(val)
		case db.TypeString:
			var length uint16
			if err := binary.Read(buf, binary.LittleEndian, &length); err != nil {
				return nil, err
			}
			strData := make([]byte, length)
			if _, err := io.ReadFull(buf, strData); err != nil {
				return nil, err
			}
			row[col.Name] = string(strData)
		}
	}
	fmt.Printf("%v\n", row)
	return row, nil
}

func SerializePage(page *Page) []byte {
	buf := make([]byte, PageSize)
	writer := bytes.NewBuffer(buf[:0])

	if err := binary.Write(writer, binary.LittleEndian, &page.Header); err != nil {
		panic("failed to write page header: " + err.Error())
	}

	for i := 0; i < MaxItemsPerPage; i++ {
		if err := binary.Write(writer, binary.LittleEndian, &page.Items[i]); err != nil {
			panic("failed to write page item: " + err.Error())
		}
	}

	if _, err := writer.Write(page.Data[:]); err != nil {
		panic("failed to write page data: " + err.Error())
	}
	final := writer.Bytes()
	if len(final) < PageSize {
		padding := make([]byte, PageSize-len(final))
		final = append(final, padding...)
	}

	return final
}

func DeserializePage(buf []byte) (*Page, error) {
	if len(buf) < PageSize {
		return nil, errors.New("buffer too small to be a valid page")
	}

	page := &Page{}
	reader := bytes.NewReader(buf)

	if err := binary.Read(reader, binary.LittleEndian, &page.Header); err != nil {
		return nil, err
	}

	for i := 0; i < MaxItemsPerPage; i++ {
		var items ItemID
		if err := binary.Read(reader, binary.LittleEndian, &items); err != nil {

			return nil, err
		}
		page.Items[i] = items
	}

	if _, err := reader.Read(page.Data[:]); err != nil {
		return nil, err
	}
	return page, nil
}
