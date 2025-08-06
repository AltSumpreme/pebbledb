package pagemanager

import (
	"pebbledb/pager"
)

type PageManager struct {
	pages      map[int]*pager.Page
	nextPageID int
}

func NewPageManager() *PageManager {
	return &PageManager{pages: make(map[int]*pager.Page), nextPageID: 0}
}

func (pm *PageManager) GetPage(PageID int) *pager.Page {
	return pm.pages[PageID]
}

func (pm *PageManager) CreateNewPage() (*pager.Page, int) {

	page := pager.PageInit()
	pageID := pm.nextPageID
	pm.pages[pageID] = page
	pm.nextPageID++
	return page, pageID

}
