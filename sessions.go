package bgx

import "sort"

// ListOptions filters session listings.
type ListOptions struct {
	// Metadata keeps only sessions whose metadata matches every entry exactly.
	Metadata map[string]string
}

// ListSessions returns every known session matching opts: live sessions merged
// with retained ended records, where a live session shadows the retained
// record for the same id. The result is sorted by id and never nil, so it can
// be marshaled directly as a JSON array.
func ListSessions(opts ListOptions) []*Info {
	byID := make(map[string]*Info)
	for _, info := range listRunning() {
		byID[info.ID] = info
	}
	for _, info := range listEnded() {
		if _, ok := byID[info.ID]; !ok {
			byID[info.ID] = info
		}
	}
	merged := make([]*Info, 0, len(byID))
	for _, info := range byID {
		merged = append(merged, info)
	}
	return filterSessions(merged, opts)
}

// ListRunning returns the live sessions matching opts, sorted by id.
func ListRunning(opts ListOptions) []*Info {
	return filterSessions(listRunning(), opts)
}

// ListEnded returns the retained ended-session records matching opts, sorted
// by id.
func ListEnded(opts ListOptions) []*Info {
	return filterSessions(listEnded(), opts)
}

// EndedRecord returns the retained record for an ended session, if one exists.
func EndedRecord(id string) (*Info, bool) {
	info, ok := endedRecord(id)
	if !ok {
		return nil, false
	}
	info.Running = false
	return info, true
}

func filterSessions(in []*Info, opts ListOptions) []*Info {
	out := []*Info{}
	for _, info := range in {
		if matchesMetadata(info, opts.Metadata) {
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
