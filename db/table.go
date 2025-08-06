package db

import (
	"errors"
	"fmt"
	"pebbledb/pagemanager"
	"strconv"
)

func NewTable(name string, columns []Column) *Table {
	return &Table{
		Name:    name,
		Columns: columns,
	}
}

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
			val := values[i]
			rowData[col.Name] = val
		default:
			return nil, fmt.Errorf("unsupported column type %s", t.Columns[i].Type)
		}
	}
	serializedData, err := SerializeRow(Row{Value: rowData}, t.Columns)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize row: %v", err)
	}

	for _, pageID := range t.PageNo {
		page := pagemanager.NewPageManager().GetPage(pageID)
		if offset, err := page.InsertTuple(serializedData); err == nil {
			return &Row{
				Value: rowData,
				TuplePointer: &TuplePointer{
					PageID: pageID,
					Offset: offset,
				},
			}, nil
		}
	}
	newPage, pageID := t.PageManager.CreateNewPage()

	offset, err := newPage.InsertTuple(serializedData)
	if err != nil {
		return nil, fmt.Errorf("failed to insert into new page: %v", err)
	}

	t.PageNo = append(t.PageNo, pageID)
	return &Row{
		Value: rowData,
		TuplePointer: &TuplePointer{
			PageID: pageID,
			Offset: offset,
		},
	}, nil
}

func (t *Table) Select(cols []Column) ([]Row, error) {

	if len(cols) != 1 || cols[0].Name != "*" {
		return nil, errors.New("only SELECT * is supported for now")
	}

	var results []Row

	for _, pageID := range t.PageNo {
		page := t.PageManager.GetPage(pageID)
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
