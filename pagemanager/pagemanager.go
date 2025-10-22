package pagemanager

import (
	"fmt"
	"os"
	"pebbledb/pager"
)

const MaxPagesInMem = 64

type PageManager struct {
	pageFiles  map[int]string //PageID -> FilePath
	cache      map[int]*pager.Page
	nextPageID int
}

func NewPageManager(pageFiles map[int]string, startPage int) *PageManager {
	return &PageManager{
		pageFiles:  pageFiles,
		cache:      make(map[int]*pager.Page),
		nextPageID: startPage,
	}
}

func (pm *PageManager) GetPage(pageID int) (*pager.Page, error) {
	if page, exists := pm.cache[pageID]; exists {
		return page, nil
	}

	path, ok := pm.pageFiles[pageID]
	if !ok {
		return nil, fmt.Errorf("page %d does not exist", pageID)
	}

	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read page %d from disk: %v", pageID, err)
	}

	page, err := pager.DeserializePage(buf)
	if err != nil {
		return nil, fmt.Errorf("failed to deserialize page %d: %v", pageID, err)
	}

	pm.addPageToCache(pageID, page)
	return page, nil

}

func (pm *PageManager) evictPage() {

	if len(pm.cache) >= MaxPagesInMem {
		var oldestPageID int
		for id := range pm.cache {
			oldestPageID = id
			break
		}
		delete(pm.cache, oldestPageID)
	}
}

// A simple FIFO eviction strategy for now
func (pm *PageManager) addPageToCache(pageID int, pg *pager.Page) {
	if len(pm.cache) >= MaxPagesInMem {
		pm.evictPage()
	}
	pm.cache[pageID] = pg
}

func (pm *PageManager) CreateNewPage() (*pager.Page, int) {

	page := pager.PageInit()
	pageID := pm.nextPageID
	pm.cache[pageID] = page
	pm.nextPageID++
	return page, pageID

}
