package db

import (
	"errors"
	"fmt"
	"pebbledb/pagemanager"
	"pebbledb/pager"
	"strconv"
)

// Initializes a new table with the given name and columns.
func NewTable(name string, columns []Column) *Table {
	return &Table{
		Name:         name,
		Columns:      columns,
		PageNo:       []int{},
		PageManager:  pagemanager.NewPageManager(make(map[int]string), 0),
		FreeSpaceMap: *NewFSM(),
	}
}

// This is the insert function for the Table struct.
func (t *Table) Insert(values []string) (*Row, error) {
	if len(t.Columns) != len(values) {
		return nil, fmt.Errorf("number of values does not match number of columns")
	}

	rowData := make(map[string]interface{}, len(t.Columns))
	for i, col := range t.Columns {
		switch col.Type {
		case TypeInt:
			val, err := strconv.Atoi(values[i])
			if err != nil {
				return nil, fmt.Errorf("invalid value for column %s: %v", col.Name, values[i])
			}
			rowData[col.Name] = val
		case TypeString:
			rowData[col.Name] = values[i]
		default:
			return nil, fmt.Errorf("unsupported column type %s", col.Type)
		}
	}

	serializedData, err := SerializeRow(Row{Value: rowData}, t.Columns)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize row: %v", err)
	}

	required := len(serializedData) + pager.ItemIDSize

	for _, class := range []int{FSMClassSmall, FSMClassMedium, FSMClassLarge, FSMClassXL} {
		if class < required {
			continue
		}

		for pageID := range t.FreeSpaceMap.buckets[class] {
			page, err := t.PageManager.GetPage(pageID)
			if err != nil {
				t.FreeSpaceMap.remove(pageID)
				continue
			}

			offset, err := page.InsertTuple(serializedData)
			if err == nil {
				t.FreeSpaceMap.update(pageID, page)

				return &Row{
					Value: rowData,
					TuplePointer: &TuplePointer{
						PageID: pageID,
						Offset: offset,
					},
				}, nil
			}

			// evict the stale entry
			t.FreeSpaceMap.remove(pageID)
		}
	}

	newPage, pageID := t.PageManager.CreateNewPage()

	offset, err := newPage.InsertTuple(serializedData)
	if err != nil {
		return nil, fmt.Errorf("failed to insert into new page: %v", err)
	}

	t.PageNo = append(t.PageNo, pageID)
	t.FreeSpaceMap.update(pageID, newPage)

	return &Row{
		Value: rowData,
		TuplePointer: &TuplePointer{
			PageID: pageID,
			Offset: offset,
		},
	}, nil
}

// Selects all rows from the table.
func (t *Table) Select(cols []Column) ([]Row, error) {

	if len(cols) != 1 || cols[0].Name != "*" {
		return nil, errors.New("only SELECT * is supported for now")
	}

	var results []Row

	for _, pageID := range t.PageNo {
		page, err := t.PageManager.GetPage(pageID)
		if err != nil {
			return nil, fmt.Errorf("failed to get page %d: %v", pageID, err)
		}
		for slot := 0; slot < int(page.Header.NumItems); slot++ {
			data, err := page.ReadTuple(slot)
			if err != nil {
				return nil, fmt.Errorf("failed to read tuple from page %d: %v", pageID, err)
			}
			row, err := DeserializeRow(data, t.Columns)
			if err != nil {
				return nil, fmt.Errorf("failed to deserialize row: %v", err)
			}
			results = append(results, Row{Value: row.Value})
		}
	}

	if len(results) == 0 {
		return nil, errors.New("no rows found")
	}
	return results, nil

}

func (t *Table) StorageUtilization() float64 {
	if len(t.PageNo) == 0 {
		return 0
	}

	var liveBytes int
	var totalBytes int

	for _, pid := range t.PageNo {
		page, err := t.PageManager.GetPage(pid)
		if err != nil {
			continue
		}
		liveBytes += page.LiveDataSize()
		totalBytes += pager.DataRegionSize
	}

	if totalBytes == 0 {
		return 0
	}
	return float64(liveBytes) / float64(totalBytes)
}

func (t *Table) ActivePageUtilization() float64 {
	var sum float64
	var count int

	for _, pid := range t.PageNo {
		page, err := t.PageManager.GetPage(pid)
		if err != nil {
			continue
		}

		if page.LiveDataSize() > 0 {
			sum += page.DataUtilization()
			count++
		}
	}

	if count == 0 {
		return 0
	}
	return sum / float64(count)
}
func (t *Table) RebuildFSM() {
	t.FreeSpaceMap = *NewFSM()
	for _, pid := range t.PageNo {
		page, _ := t.PageManager.GetPage(pid)
		t.FreeSpaceMap.update(pid, page)
	}
}
