package consensus

import "BluePods/internal/logger"

// ScanResult holds the result of a background object scan.
type ScanResult struct {
	NeedFetch []Hash // NeedFetch are objects this node should hold but doesn't have
	CanDrop   []Hash // CanDrop are objects this node no longer needs to hold
}

// objectScanner performs background object scanning after epoch transitions.
// It iterates all tracked objects and checks holder status against the new epoch set.
type objectScanner struct {
	tracker  *objectTracker
	isHolder func([32]byte, uint16) bool
	hasLocal func([32]byte) bool // hasLocal checks if the object exists in local state
}

// ScanObjects creates a scanner and scans all tracked objects.
// isHolder checks if this node should hold an object.
// hasLocal checks if an object exists in local state.
// Returns objects to fetch and objects that can be dropped.
func (d *DAG) ScanObjects(isHolder func([32]byte, uint16) bool, hasLocal func([32]byte) bool) ScanResult {
	s := &objectScanner{
		tracker:  d.tracker,
		isHolder: isHolder,
		hasLocal: hasLocal,
	}

	return s.scanObjects()
}

// scanObjects iterates all tracked objects and determines what to fetch/drop.
// This runs in background after an epoch transition.
func (s *objectScanner) scanObjects() ScanResult {
	var result ScanResult

	entries := s.tracker.Export()

	for _, entry := range entries {
		isHolder := s.isHolder(entry.ID, entry.Replication)
		hasLocal := s.hasLocal != nil && s.hasLocal(entry.ID)

		if isHolder && !hasLocal {
			result.NeedFetch = append(result.NeedFetch, entry.ID)
		}

		if !isHolder && hasLocal {
			result.CanDrop = append(result.CanDrop, entry.ID)
		}
	}

	logger.Info("epoch scan complete",
		"totalObjects", len(entries),
		"needFetch", len(result.NeedFetch),
		"canDrop", len(result.CanDrop),
	)

	return result
}
