package db

import "pebbledb/pager"

const (
	FSMClassSmall  = 256
	FSMClassMedium = 512
	FSMClassLarge  = 1024
	FSMClassXL     = 2048
)

type FSM struct {
	buckets map[int]map[int]struct{}
}

func NewFSM() *FSM {
	return &FSM{
		buckets: map[int]map[int]struct{}{
			FSMClassSmall:  {},
			FSMClassMedium: {},
			FSMClassLarge:  {},
			FSMClassXL:     {},
		},
	}
}

func ClassifyFreeSpace(free int) int {
	switch {
	case free >= FSMClassSmall:
		return FSMClassSmall

	case free >= FSMClassMedium:
		return FSMClassMedium

	case free >= FSMClassLarge:
		return FSMClassLarge

	case free >= FSMClassXL:
		return FSMClassXL
	default:
		return 0
	}
}

func (fsm *FSM) remove(pageID int) {
	for _, bucket := range fsm.buckets {
		delete(bucket, pageID)
	}
}

func (fsm *FSM) update(PageID int, page *pager.Page) {
	fsm.remove(PageID)
	free := page.FreeSpace()
	class := ClassifyFreeSpace(free)

	if class == 0 {
		return
	}
	fsm.buckets[class][PageID] = struct{}{}
}
