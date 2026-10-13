package orderstream

import (
	"container/list"
	"strings"
	"sync"
	"time"
)

const (
	recentFillAverageTTL             = 20 * time.Minute
	recentFillAverageExpireScanLimit = 32
)

// RecentFillAverage is a session-independent snapshot of cumulative fill
// average price. It never stores API credentials.
type RecentFillAverage struct {
	Key              Key
	ClientOrderID    string
	VenueOrderID     string
	Status           Status
	CumulativeFilled string
	AveragePrice     string
	EventTime        time.Time
	Sequence         int64
	ExpiresAt        time.Time
}

type recentFillNode struct {
	value RecentFillAverage
	elem  *list.Element
}

type recentFillCache struct {
	mu       sync.Mutex
	byClient map[string]*recentFillNode
	byVenue  map[string]*recentFillNode
	order    *list.List
}

func newRecentFillCache() *recentFillCache {
	return &recentFillCache{
		byClient: make(map[string]*recentFillNode),
		byVenue:  make(map[string]*recentFillNode),
		order:    list.New(),
	}
}

func (m *Manager) rememberRecentFillAverage(key Key, update Update) {
	if m == nil || m.fillAverages == nil {
		return
	}
	normalized, err := key.normalized()
	if err != nil {
		return
	}
	m.fillAverages.remember(m.now(), normalized, update)
}

func (m *Manager) LookupRecentFillAverage(
	key Key,
	clientOrderID string,
	venueOrderID string,
) (RecentFillAverage, bool) {
	if m == nil || m.fillAverages == nil {
		return RecentFillAverage{}, false
	}
	normalized, err := key.normalized()
	if err != nil {
		return RecentFillAverage{}, false
	}
	return m.fillAverages.lookup(m.now(), normalized, clientOrderID, venueOrderID)
}

func (c *recentFillCache) remember(now time.Time, key Key, update Update) {
	if c == nil {
		return
	}
	clientID := strings.TrimSpace(update.ClientOrderID)
	venueID := strings.TrimSpace(update.VenueOrderID)
	if clientID == "" && venueID == "" {
		return
	}
	average := parseDecimal(update.AveragePrice)
	filled := parseDecimal(update.CumulativeFilled)
	if !average.IsPositive() || !filled.IsPositive() {
		return
	}
	next := RecentFillAverage{
		Key:              key,
		ClientOrderID:    clientID,
		VenueOrderID:     venueID,
		Status:           update.Status,
		CumulativeFilled: strings.TrimSpace(update.CumulativeFilled),
		AveragePrice:     strings.TrimSpace(update.AveragePrice),
		EventTime:        update.EventTime,
		Sequence:         update.Sequence,
		ExpiresAt:        now.Add(recentFillAverageTTL),
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireSomeLocked(now)
	node := c.findLocked(key, clientID, venueID)
	if node != nil {
		if !fillAverageNewer(next, node.value) {
			return
		}
		c.reindexLocked(node, next)
		c.order.MoveToBack(node.elem)
		return
	}
	node = &recentFillNode{value: next}
	node.elem = c.order.PushBack(node)
	c.indexLocked(node)
}

func (c *recentFillCache) lookup(
	now time.Time,
	key Key,
	clientOrderID string,
	venueOrderID string,
) (RecentFillAverage, bool) {
	if c == nil {
		return RecentFillAverage{}, false
	}
	clientID := strings.TrimSpace(clientOrderID)
	venueID := strings.TrimSpace(venueOrderID)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireSomeLocked(now)
	node := c.findLocked(key, clientID, venueID)
	if node == nil {
		return RecentFillAverage{}, false
	}
	if !now.Before(node.value.ExpiresAt) {
		c.removeNodeLocked(node)
		return RecentFillAverage{}, false
	}
	if !fillAverageIDsMatch(node.value, clientID, venueID) {
		return RecentFillAverage{}, false
	}
	return node.value, true
}

func (c *recentFillCache) expireSomeLocked(now time.Time) {
	scanned := 0
	for scanned < recentFillAverageExpireScanLimit && c.order.Len() > 0 {
		node, _ := c.order.Front().Value.(*recentFillNode)
		if node == nil {
			c.order.Remove(c.order.Front())
			scanned++
			continue
		}
		if now.Before(node.value.ExpiresAt) {
			return
		}
		c.removeNodeLocked(node)
		scanned++
	}
}

func (c *recentFillCache) findLocked(key Key, clientID, venueID string) *recentFillNode {
	if clientID != "" {
		if node := c.byClient[fillAverageIndexKey(key, clientID)]; node != nil {
			return node
		}
	}
	if venueID != "" {
		return c.byVenue[fillAverageIndexKey(key, venueID)]
	}
	return nil
}

func (c *recentFillCache) reindexLocked(node *recentFillNode, next RecentFillAverage) {
	prev := node.value
	if prev.ClientOrderID != "" && prev.ClientOrderID != next.ClientOrderID {
		old := fillAverageIndexKey(prev.Key, prev.ClientOrderID)
		if c.byClient[old] == node {
			delete(c.byClient, old)
		}
	}
	if prev.VenueOrderID != "" && prev.VenueOrderID != next.VenueOrderID {
		old := fillAverageIndexKey(prev.Key, prev.VenueOrderID)
		if c.byVenue[old] == node {
			delete(c.byVenue, old)
		}
	}
	node.value = next
	c.indexLocked(node)
}

func (c *recentFillCache) indexLocked(node *recentFillNode) {
	if node.value.ClientOrderID != "" {
		c.byClient[fillAverageIndexKey(node.value.Key, node.value.ClientOrderID)] = node
	}
	if node.value.VenueOrderID != "" {
		c.byVenue[fillAverageIndexKey(node.value.Key, node.value.VenueOrderID)] = node
	}
}

func (c *recentFillCache) removeNodeLocked(node *recentFillNode) {
	if node.elem != nil {
		c.order.Remove(node.elem)
		node.elem = nil
	}
	if node.value.ClientOrderID != "" {
		key := fillAverageIndexKey(node.value.Key, node.value.ClientOrderID)
		if c.byClient[key] == node {
			delete(c.byClient, key)
		}
	}
	if node.value.VenueOrderID != "" {
		key := fillAverageIndexKey(node.value.Key, node.value.VenueOrderID)
		if c.byVenue[key] == node {
			delete(c.byVenue, key)
		}
	}
}

func fillAverageIndexKey(key Key, id string) string {
	return key.Venue + "\x00" + key.Account + "\x00" + key.Product + "\x00" + id
}

func fillAverageNewer(next, prev RecentFillAverage) bool {
	if next.EventTime.After(prev.EventTime) {
		return true
	}
	if next.EventTime.Before(prev.EventTime) {
		return false
	}
	return next.Sequence > prev.Sequence
}

func fillAverageIDsMatch(snapshot RecentFillAverage, clientID, venueID string) bool {
	if clientID == "" && venueID == "" {
		return false
	}
	if clientID != "" && snapshot.ClientOrderID != "" && clientID != snapshot.ClientOrderID {
		return false
	}
	if venueID != "" && snapshot.VenueOrderID != "" && venueID != snapshot.VenueOrderID {
		return false
	}
	if clientID != "" && snapshot.ClientOrderID == clientID {
		return true
	}
	if venueID != "" && snapshot.VenueOrderID == venueID {
		return true
	}
	return false
}
