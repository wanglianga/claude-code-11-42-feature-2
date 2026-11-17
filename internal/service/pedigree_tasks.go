package service

import "sync"

// In-memory registry of on-demand pedigree evaluation tasks. The authoritative
// calculation results live in the append-only pedigree_calcs table; this map
// only tracks task progress so callers can poll a running/finished handle.
var (
	tasksMu       sync.Mutex
	pedigreeTasks = map[string]*pedigreeTask{}
)
