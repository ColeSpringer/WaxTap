package waxtap

import (
	"slices"

	"github.com/colespringer/waxlabel"
)

// TagCarry itemizes what carrying a local input's embedded metadata onto the
// output did with each piece: the facts the tag-carry-incomplete warning tells
// in prose, one item per field and per set, so a caller can ask whether the
// chapters landed without parsing a sentence. Result.TagCarry is nil when no
// carry ran: a YouTube source (the embed options write metadata instead), a
// measure-only run (the input is delivered as it is), an input carrying no
// metadata, or one WaxLabel cannot read.
type TagCarry struct {
	// Items lists every piece of the input's metadata with its fate, in the
	// transfer's order: the fields, then the picture, chapter, and
	// synced-lyrics sets. A set can appear twice, as its carried part and its
	// downgraded or dropped part, each item standing for the pieces its Count
	// states.
	Items []CarryItem
	// Error is set when the carry itself could not run: the output could not
	// be read back, or the transfer could not be prepared or written. Items is
	// then empty and the output holds none of the input's metadata. The same
	// text reaches Warnings under WarnTagCarry.
	Error string
}

// CarryKind names what a CarryItem describes.
type CarryKind uint8

const (
	CarryField        CarryKind = iota // one tag field, named by CarryItem.Key
	CarryPictures                      // the cover-art set
	CarryChapters                      // the chapter set
	CarrySyncedLyrics                  // the synced-lyrics sets
)

// String returns the kind's stable spelling, the one --json uses.
func (k CarryKind) String() string {
	switch k {
	case CarryField:
		return "field"
	case CarryPictures:
		return "pictures"
	case CarryChapters:
		return "chapters"
	case CarrySyncedLyrics:
		return "synced-lyrics"
	}
	return "unknown"
}

// CarryDisposition grades how a piece of metadata survived the carry.
type CarryDisposition uint8

const (
	// DispositionCarried means the output holds it as the input had it.
	DispositionCarried CarryDisposition = iota
	// DispositionDowngraded means the output holds it with reduced fidelity;
	// Reason says how.
	DispositionDowngraded
	// DispositionDropped means the output format cannot hold it; Reason says
	// why.
	DispositionDropped
	// DispositionExcluded means policy left it off: a value describing the
	// input's own audio (ReplayGain, an encoder stamp) that a re-encode or cut
	// made wrong. A pure remux keeps that audio and restores such values,
	// which then report DispositionCarried.
	DispositionExcluded
	// DispositionRemoved means a cut took every piece of a set along with the
	// audio it described, so the output holds none; Removed counts them. It
	// is the cut's doing, not a loss to the format, and a set the cut only
	// thinned stays DispositionCarried with its Removed count.
	DispositionRemoved
)

// String returns the disposition's stable spelling, the one --json uses.
func (d CarryDisposition) String() string {
	switch d {
	case DispositionCarried:
		return "carried"
	case DispositionDowngraded:
		return "downgraded"
	case DispositionDropped:
		return "dropped"
	case DispositionExcluded:
		return "excluded"
	case DispositionRemoved:
		return "removed"
	}
	return "unknown"
}

// CarryItem is one piece of metadata's fate.
type CarryItem struct {
	Kind CarryKind
	// Key is the tag key of a CarryField item ("TITLE"); empty for the sets.
	Key string
	// Count is what the item stands for: a field's values, or the pictures,
	// chapters, or synced-lyrics sets in a set. For a set that landed it is
	// the output's count, a cut's removals taken out; for one that did not,
	// what the carry would have written: the input's, or after a cut the
	// remap's, with Removed the rest.
	Count       int
	Disposition CarryDisposition
	// Reason says why, for anything but DispositionCarried and
	// DispositionRemoved, in the words the warning uses.
	Reason string
	// Removed counts what a cut took along with the audio it described:
	// chapters whose whole span was removed, or synced-lyric lines whose
	// instants were. They are not carry losses (the output has no audio for
	// them), so a set that kept any piece stays DispositionCarried; one the
	// cut emptied reports DispositionRemoved with Count 0.
	Removed int
}

// carryItems maps a transfer report onto CarryItems, one for one.
func carryItems(r waxlabel.TransferReport) []CarryItem {
	items := make([]CarryItem, 0, len(r.Items))
	for _, it := range r.Items {
		items = append(items, CarryItem{
			Kind:        carryKind(it.Kind),
			Key:         string(it.Key),
			Count:       it.Count,
			Disposition: carryDisposition(it.Disposition),
			Reason:      it.Reason,
		})
	}
	return items
}

func carryKind(k waxlabel.TransferKind) CarryKind {
	switch k {
	case waxlabel.TransferPicture:
		return CarryPictures
	case waxlabel.TransferChapter:
		return CarryChapters
	case waxlabel.TransferSyncedLyric:
		return CarrySyncedLyrics
	}
	return CarryField
}

func carryDisposition(d waxlabel.Disposition) CarryDisposition {
	switch d {
	case waxlabel.Lossy:
		return DispositionDowngraded
	case waxlabel.Dropped:
		return DispositionDropped
	case waxlabel.Excluded:
		return DispositionExcluded
	}
	return DispositionCarried
}

// remapped records a cut's remap of a set. Where the set landed, its carried
// and downgraded items fold into one that states what the output holds and
// what the cut removed, downgraded if any part was. A set the cut emptied
// never entered the transfer and has no item, so one is added, in the
// transfer's place for it, reporting it removed outright. A set the
// destination dropped keeps its item, whose Count is the remap's (the list
// the transfer graded), and takes what the cut removed beside it.
func (tc *TagCarry) remapped(kind CarryKind, r cutRemap, landed bool) {
	if !landed {
		if r.kept == 0 && r.removed > 0 {
			tc.insert(CarryItem{Kind: kind, Disposition: DispositionRemoved, Removed: r.removed})
			return
		}
		for i, it := range tc.Items {
			if it.Kind == kind {
				tc.Items[i].Removed = r.removed
				break
			}
		}
		return
	}
	isLanded := func(it CarryItem) bool {
		return it.Kind == kind && (it.Disposition == DispositionCarried || it.Disposition == DispositionDowngraded)
	}
	merged := CarryItem{Kind: kind, Count: r.kept, Removed: r.removed}
	for _, it := range tc.Items {
		if isLanded(it) && it.Disposition == DispositionDowngraded {
			merged.Disposition, merged.Reason = DispositionDowngraded, it.Reason
		}
	}
	out := make([]CarryItem, 0, len(tc.Items))
	placed := false
	for _, it := range tc.Items {
		switch {
		case !isLanded(it):
			out = append(out, it)
		case !placed:
			out = append(out, merged)
			placed = true
		}
	}
	tc.Items = out
}

// insert places it before the first item of a later kind, so Items keeps the
// transfer's order (fields, then pictures, chapters, synced lyrics), which is
// the order CarryKind's values run in.
func (tc *TagCarry) insert(it CarryItem) {
	at := len(tc.Items)
	for i, have := range tc.Items {
		if have.Kind > it.Kind {
			at = i
			break
		}
	}
	tc.Items = slices.Insert(tc.Items, at, it)
}

// restored marks the excluded fields a remux put back as carried.
func (tc *TagCarry) restored(keys []string) {
	for i, it := range tc.Items {
		if it.Kind == CarryField && it.Disposition == DispositionExcluded {
			for _, k := range keys {
				if it.Key == k {
					tc.Items[i].Disposition, tc.Items[i].Reason = DispositionCarried, ""
					break
				}
			}
		}
	}
}
