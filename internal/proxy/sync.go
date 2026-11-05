package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
)

// Each RID identifies a filtered snapshot, not the qBittorrent session's RID.
// Bounded history allows clients to retry an older RID without losing removals.
type syncRIDKey struct{}
type syncSnapshot struct {
	session                              [32]byte
	torrents, categories, tags, trackers map[string]struct{}
}
type syncHistory struct {
	mu      sync.Mutex
	entries map[uint32]syncSnapshot
	order   []uint32
}

func keySet(data object, key string, array bool) (map[string]struct{}, error) {
	result := make(map[string]struct{})
	raw, ok := data[key]
	if !ok {
		return result, nil
	}
	if array {
		var values []string
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, fmt.Errorf("decoding %s: %w", key, err)
		}
		for _, value := range values {
			result[value] = struct{}{}
		}
	} else {
		var values map[string]json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, fmt.Errorf("decoding %s: %w", key, err)
		}
		for value := range values {
			result[value] = struct{}{}
		}
	}
	return result, nil
}

func removed(old, current map[string]struct{}) []string {
	values := make([]string, 0)
	for key := range old {
		if _, ok := current[key]; !ok {
			values = append(values, key)
		}
	}
	slices.Sort(values)
	return values
}

func (h *handler) syncResponse(r *http.Request, body []byte) ([]byte, error) {
	var data object
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("decoding sync: %w", err)
	}
	var full bool
	if err := json.Unmarshal(data["full_update"], &full); err != nil || !full {
		return nil, errors.New("upstream did not provide a full sync snapshot")
	}
	var torrents map[string]object
	if raw, ok := data["torrents"]; ok {
		if err := json.Unmarshal(raw, &torrents); err != nil {
			return nil, fmt.Errorf("decoding sync torrents: %w", err)
		}
	}
	if torrents == nil {
		torrents = make(map[string]object)
	}
	for hash, item := range torrents {
		if err := h.rewriteTorrent(r, hash, item); err != nil {
			slog.Warn("hiding torrent", "hash", hash, "reason", err)
			delete(torrents, hash)
		}
	}
	var err error
	data["torrents"], err = json.Marshal(torrents)
	if err != nil {
		return nil, fmt.Errorf("encoding sync torrents: %w", err)
	}
	current := syncSnapshot{session: sha256.Sum256([]byte(r.Header.Get("Cookie"))), torrents: make(map[string]struct{})}
	for hash := range torrents {
		current.torrents[hash] = struct{}{}
	}
	if raw, ok := data["trackers"]; ok {
		var trackers map[string][]string
		if err := json.Unmarshal(raw, &trackers); err != nil {
			return nil, fmt.Errorf("decoding trackers: %w", err)
		}
		for tracker, hashes := range trackers {
			filtered := make([]string, 0, len(hashes))
			for _, hash := range hashes {
				if _, visible := current.torrents[hash]; visible {
					filtered = append(filtered, hash)
				}
			}
			if len(filtered) == 0 {
				delete(trackers, tracker)
			} else {
				trackers[tracker] = filtered
			}
		}
		data["trackers"], err = json.Marshal(trackers)
		if err != nil {
			return nil, fmt.Errorf("encoding trackers: %w", err)
		}
	}
	if current.categories, err = keySet(data, "categories", false); err != nil {
		return nil, err
	}
	if current.tags, err = keySet(data, "tags", true); err != nil {
		return nil, err
	}
	if current.trackers, err = keySet(data, "trackers", false); err != nil {
		return nil, err
	}

	h.syncState.mu.Lock()
	defer h.syncState.mu.Unlock()
	if h.syncState.entries == nil {
		h.syncState.entries = make(map[uint32]syncSnapshot)
	}
	ridValue, ok := r.Context().Value(syncRIDKey{}).(string)
	if !ok {
		return nil, errors.New("missing client sync id")
	}
	previousRID, parseErr := strconv.ParseUint(ridValue, 10, 32)
	if parseErr != nil {
		previousRID = 0
	}
	previous, known := h.syncState.entries[uint32(previousRID)]
	known = known && previous.session == current.session
	if known {
		// Incremental tag arrays contain additions, not the entire set.
		data["tags"], err = json.Marshal(removed(current.tags, previous.tags))
		if err != nil {
			return nil, fmt.Errorf("encoding tags: %w", err)
		}
		for _, change := range []struct {
			key           string
			before, after map[string]struct{}
		}{
			{"torrents_removed", previous.torrents, current.torrents}, {"categories_removed", previous.categories, current.categories},
			{"tags_removed", previous.tags, current.tags}, {"trackers_removed", previous.trackers, current.trackers},
		} {
			data[change.key], err = json.Marshal(removed(change.before, change.after))
			if err != nil {
				return nil, fmt.Errorf("encoding %s: %w", change.key, err)
			}
		}
		data["full_update"] = json.RawMessage("false")
	} else {
		data["full_update"] = json.RawMessage("true")
		for _, key := range []string{"torrents_removed", "categories_removed", "tags_removed", "trackers_removed"} {
			delete(data, key)
		}
	}
	var rid uint32
	for rid == 0 {
		var random [4]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, fmt.Errorf("creating sync id: %w", err)
		}
		candidate := binary.BigEndian.Uint32(random[:]) & 0x7fffffff
		if _, exists := h.syncState.entries[candidate]; !exists {
			rid = candidate
		}
	}
	data["rid"] = json.RawMessage(strconv.FormatUint(uint64(rid), 10))
	result, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("encoding sync response: %w", err)
	}
	if len(h.syncState.order) >= 128 {
		delete(h.syncState.entries, h.syncState.order[0])
		h.syncState.order = h.syncState.order[1:]
	}
	h.syncState.entries[rid] = current
	h.syncState.order = append(h.syncState.order, rid)
	return result, nil
}
