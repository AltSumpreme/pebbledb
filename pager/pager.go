package pager

import (
	"bytes"
	"encoding/binary"
	"errors"
)

const (
	PageSize         = 4096 //4KB
	PageHeaderSize   = 16
	ItemIDSize       = 6
	MaxItemsPerPage  = 128
	SpecialSpaceSize = 0 // Reserved for future user for indexing metadata
	DataRegionSize   = PageSize - PageHeaderSize - MaxItemsPerPage*ItemIDSize - SpecialSpaceSize
)

// Currently supports only the PageType table but will soon support the others.
const (
	PageTypeTable         uint8 = 1
	PageTypeBTreeInternal       = 2
	PageTypeBTreeLeaf           = 3
	PageTypeRun                 = 4
)

type PageHeader struct {
	LSN      uint64 // Future WAL support
	NumItems uint16
	PdLower  uint16
	PdUpper  uint16
	PageType uint8
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

func PageInit() *Page {
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
		return -2, errors.New("page is full")
	}

	freeSpace := p.FreeSpace()
	requiredSpace := len(record) + ItemIDSize
	if requiredSpace > freeSpace {
		return -1, errors.New("not enough space in page")
	}

	newUpper := p.Header.PdUpper - uint16(len(record))
	copy(p.Data[newUpper:], record)

	slot := p.findFreeSlot()
	if slot == -1 {
		slot = int(p.Header.NumItems)
		p.Header.NumItems++
		p.Header.PdLower += ItemIDSize
	}
	p.Items[slot] = ItemID{
		Offset:      newUpper,
		Length:      uint16(len(record)),
		DeletedFlag: 1, // Mark as not deleted
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

func (p *Page) GetAllTuples() ([][]byte, error) {

	var tuples [][]byte

	for i := 0; i < int(p.Header.NumItems); i++ {
		item := p.Items[i]
		if item.DeletedFlag == 0 {
			continue
		}
		data := make([]byte, item.Length)
		copy(data, p.Data[item.Offset:item.Offset+item.Length])
		tuples = append(tuples, data)
	}
	return tuples, nil
}

func (p *Page) findFreeSlot() int {
	for i := 0; i < int(p.Header.NumItems); i++ {
		if p.Items[i].DeletedFlag == 0 {
			return i
		}
	}
	return -1
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

// Helper function to give telemetry regarding Page utilization

func (p *Page) DataUtilization() float64 {
	used := DataRegionSize - int(p.Header.PdUpper)
	return float64(used) / float64(DataRegionSize)
}

func (p *Page) FreeSpace() int {
	return int(p.Header.PdUpper) - int(p.Header.PdLower)
}
func (p *Page) LiveDataSize() int {
	var sum int
	for i := 0; i < int(p.Header.NumItems); i++ {
		if p.Items[i].DeletedFlag == 1 {
			sum += int(p.Items[i].Length)
		}
	}
	return sum
}
