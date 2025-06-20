package pager

import "os"

const PageSize = 4096 // 4KB page size

type Page struct {
	ID   int
	Data [PageSize]byte
}
type Pager struct {
	file       *os.File
	pageSize   int
	nextPageID int
}

func PagerInit(file *os.File) *Pager {
	return &Pager{
		file:       file,
		pageSize:   PageSize,
		nextPageID: 0,
	}
}

func (p *Pager) NewPage() *Page {
	page := &Page{
		ID:   p.nextPageID,
		Data: [PageSize]byte{},
	}
	p.nextPageID++
	return page
}
func (p *Pager) WritePage(page *Page) error {

	offset := int64(page.ID) * PageSize
	_, err := p.file.WriteAt(page.Data[:], offset)
	if err != nil {
		return err
	}

	return nil
}

func (p *Pager) ReadPage(pageID int) (*Page, error) {
	page := &Page{
		ID:   pageID,
		Data: [PageSize]byte{},
	}

	offset := int64(pageID) * PageSize
	_, err := p.file.ReadAt(page.Data[:], offset)
	if err != nil {
		return nil, err
	}

	return page, nil
}

func (p *Pager) Close() error {
	if p.file != nil {
		return p.file.Close()
	}
	return nil
}
