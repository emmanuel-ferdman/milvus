package stats

import (
	"fmt"
	"sync"

	"github.com/cockroachdb/errors"

	"github.com/milvus-io/milvus/internal/streamingnode/server/wal/interceptors/shard/utils"
)

var (
	ErrNotEnoughSpace       = errors.New("not enough space")
	ErrTooLargeInsert       = errors.New("insert too large")
	NewSegmentStatFromProto = utils.NewSegmentStatFromProto
	NewProtoFromSegmentStat = utils.NewProtoFromSegmentStat
)

type (
	SegmentStats         = utils.SegmentStats
	InsertMetrics        = utils.InsertMetrics
	SyncOperationMetrics = utils.SyncOperationMetrics
)

// StatsManager is the manager of stats.
// It manages the insert stats of all segments, used to check if a segment has enough space to insert or should be sealed.
// If there will be a lock contention, we can optimize it by apply lock per segment.
type StatsManager struct {
	mu            sync.Mutex
	totalStats    InsertMetrics
	pchannelStats map[string]*InsertMetrics
	vchannelStats map[string]*InsertMetrics
	segmentStats  map[int64]*SegmentStats       // map[SegmentID]SegmentStats
	segmentIndex  map[int64]SegmentBelongs      // map[SegmentID]channels
	pchannelIndex map[string]map[int64]struct{} // map[PChannel]SegmentID
	sealNotifier  *SealSignalNotifier
}

type SegmentBelongs struct {
	PChannel     string
	VChannel     string
	CollectionID int64
	PartitionID  int64
	SegmentID    int64
}

// NewStatsManager creates a new stats manager.
func NewStatsManager() *StatsManager {
	return &StatsManager{
		mu:            sync.Mutex{},
		totalStats:    InsertMetrics{},
		pchannelStats: make(map[string]*InsertMetrics),
		vchannelStats: make(map[string]*InsertMetrics),
		segmentStats:  make(map[int64]*SegmentStats),
		segmentIndex:  make(map[int64]SegmentBelongs),
		pchannelIndex: make(map[string]map[int64]struct{}),
		sealNotifier:  NewSealSignalNotifier(),
	}
}

// RegisterNewGrowingSegment registers a new growing segment.
// delegate the stats management to stats manager.
func (m *StatsManager) RegisterNewGrowingSegment(belongs SegmentBelongs, segmentID int64, stats *SegmentStats) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.segmentStats[segmentID]; ok {
		panic(fmt.Sprintf("register a segment %d that already exist, critical bug", segmentID))
	}

	m.segmentStats[segmentID] = stats
	m.segmentIndex[segmentID] = belongs
	if _, ok := m.pchannelIndex[belongs.PChannel]; !ok {
		m.pchannelIndex[belongs.PChannel] = make(map[int64]struct{})
	}
	m.pchannelIndex[belongs.PChannel][segmentID] = struct{}{}
	m.totalStats.Collect(stats.Insert)
	if _, ok := m.pchannelStats[belongs.PChannel]; !ok {
		m.pchannelStats[belongs.PChannel] = &InsertMetrics{}
	}
	m.pchannelStats[belongs.PChannel].Collect(stats.Insert)

	if _, ok := m.vchannelStats[belongs.VChannel]; !ok {
		m.vchannelStats[belongs.VChannel] = &InsertMetrics{}
	}
	m.vchannelStats[belongs.VChannel].Collect(stats.Insert)
}

// AllocRows alloc number of rows on current segment.
func (m *StatsManager) AllocRows(segmentID int64, insert InsertMetrics) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Must be exist, otherwise it's a bug.
	info, ok := m.segmentIndex[segmentID]
	if !ok {
		panic(fmt.Sprintf("alloc rows on a segment %d that not exist", segmentID))
	}
	stat := m.segmentStats[segmentID]
	inserted := stat.AllocRows(insert)

	// update the total stats if inserted.
	if inserted {
		m.totalStats.Collect(insert)
		if _, ok := m.pchannelStats[info.PChannel]; !ok {
			m.pchannelStats[info.PChannel] = &InsertMetrics{}
		}
		m.pchannelStats[info.PChannel].Collect(insert)
		if _, ok := m.vchannelStats[info.VChannel]; !ok {
			m.vchannelStats[info.VChannel] = &InsertMetrics{}
		}
		m.vchannelStats[info.VChannel].Collect(insert)
		return nil
	}

	if stat.ShouldBeSealed() {
		// notify seal manager to do seal the segment if stat reach the limit.
		m.sealNotifier.AddAndNotify(info)
	}
	if stat.IsEmpty() {
		return ErrTooLargeInsert
	}
	return ErrNotEnoughSpace
}

// SealNotifier returns the seal notifier.
func (m *StatsManager) SealNotifier() *SealSignalNotifier {
	// no lock here, because it's read only.
	return m.sealNotifier
}

// GetStatsOfSegment gets the stats of segment.
func (m *StatsManager) GetStatsOfSegment(segmentID int64) *SegmentStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.segmentStats[segmentID].Copy()
}

// UpdateOnSync updates the stats of segment on sync.
// It's an async update operation, so it's not necessary to do success.
func (m *StatsManager) UpdateOnSync(segmentID int64, syncMetric SyncOperationMetrics) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Must be exist, otherwise it's a bug.
	if _, ok := m.segmentIndex[segmentID]; !ok {
		return
	}
	m.segmentStats[segmentID].UpdateOnSync(syncMetric)

	// binlog counter is updated, notify seal manager to do seal scanning.
	m.sealNotifier.AddAndNotify(m.segmentIndex[segmentID])
}

// UnregisterSealedSegment unregisters the sealed segment.
func (m *StatsManager) UnregisterSealedSegment(segmentID int64) *SegmentStats {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.unregisterSealedSegment(segmentID)
}

func (m *StatsManager) unregisterSealedSegment(segmentID int64) *SegmentStats {
	// Must be exist, otherwise it's a bug.
	info, ok := m.segmentIndex[segmentID]
	if !ok {
		panic(fmt.Sprintf("unregister a segment %d that not exist, critical bug", segmentID))
	}

	stats := m.segmentStats[segmentID]

	m.totalStats.Subtract(stats.Insert)
	delete(m.segmentStats, segmentID)
	delete(m.segmentIndex, segmentID)
	if _, ok := m.pchannelIndex[info.PChannel]; ok {
		delete(m.pchannelIndex[info.PChannel], segmentID)
		if len(m.pchannelIndex[info.PChannel]) == 0 {
			delete(m.pchannelIndex, info.PChannel)
		}
	}

	if _, ok := m.pchannelStats[info.PChannel]; ok {
		m.pchannelStats[info.PChannel].Subtract(stats.Insert)
		if m.pchannelStats[info.PChannel].BinarySize == 0 {
			delete(m.pchannelStats, info.PChannel)
		}
	}
	if _, ok := m.vchannelStats[info.VChannel]; ok {
		m.vchannelStats[info.VChannel].Subtract(stats.Insert)
		if m.vchannelStats[info.VChannel].BinarySize == 0 {
			delete(m.vchannelStats, info.VChannel)
		}
	}
	return stats
}

// UnregisterAllStatsOnPChannel unregisters all stats on pchannel.
func (m *StatsManager) UnregisterAllStatsOnPChannel(pchannel string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	segmentIDs, ok := m.pchannelIndex[pchannel]
	if !ok {
		return 0
	}
	for segmentID := range segmentIDs {
		m.unregisterSealedSegment(segmentID)
	}
	return len(segmentIDs)
}

// SealByTotalGrowingSegmentsSize seals the largest growing segment
// if the total size of growing segments in ANY vchannel exceeds the threshold.
func (m *StatsManager) SealByTotalGrowingSegmentsSize(vchannelThreshold uint64) *SegmentBelongs {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, metrics := range m.vchannelStats {
		if metrics.BinarySize >= vchannelThreshold {
			var (
				largestSegment     int64  = 0
				largestSegmentSize uint64 = 0
			)
			for segmentID, stats := range m.segmentStats {
				if stats.Insert.BinarySize > largestSegmentSize {
					largestSegmentSize = stats.Insert.BinarySize
					largestSegment = segmentID
				}
			}
			belongs, ok := m.segmentIndex[largestSegment]
			if !ok {
				panic("unrechable: the segmentID should always be found in segmentIndex")
			}
			return &belongs
		}
	}
	return nil
}
