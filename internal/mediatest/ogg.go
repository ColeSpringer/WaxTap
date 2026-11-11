package mediatest

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
)

// FrontTrimVorbis reshapes an Ogg Vorbis stream that starts at time zero into
// one that starts before it, the layout Vorbis I section A.2 gives a front
// trim: the first two audio packets alone on a page whose granule is short of
// the samples they hold, with every later granule moved back by the same
// amount. It returns the stream and the trim, in samples. A decoder discards
// that many samples from the front, and a container that cannot state a
// negative start cannot carry the packets unchanged, which is what a packet
// copy into Ogg declines over.
//
// The source must be the shape WaxFlow's Ogg muxer writes: the first audio
// packet alone on its page, the second opening the next. Anything else is an
// error rather than a guess.
func FrontTrimVorbis(ogg []byte) ([]byte, int64, error) {
	// The second audio packet's duration is what its page's granule has to
	// fall short of; the first primes the overlap and delivers nothing.
	demux, _, err := format.OpenDemuxer(container.BytesSource(ogg), "ogg", nil)
	if err != nil {
		return nil, 0, fmt.Errorf("FrontTrimVorbis: open the source: %w", err)
	}
	var pkt container.Packet
	var held int64
	for range 2 {
		if err := demux.ReadPacket(&pkt); err != nil {
			return nil, 0, fmt.Errorf("FrontTrimVorbis: read the first packets: %w", err)
		}
		held += pkt.Dur
	}
	trim := held / 2
	if trim <= 0 {
		return nil, 0, fmt.Errorf("FrontTrimVorbis: the first two packets hold %d samples, too few to trim", held)
	}

	pages, err := oggPages(ogg)
	if err != nil {
		return nil, 0, fmt.Errorf("FrontTrimVorbis: %w", err)
	}
	// The three header packets come first; the page after the one the third
	// completes on is the first audio page.
	first, done := -1, 0
	for i, p := range pages {
		if done == 3 {
			first = i
			break
		}
		done += p.packets()
	}
	if first < 0 || first+1 >= len(pages) || done != 3 {
		return nil, 0, fmt.Errorf("FrontTrimVorbis: no audio page after the headers")
	}
	a, b := &pages[first], &pages[first+1]
	if a.packets() != 1 || a.continued() || b.continued() || b.packets() < 2 {
		return nil, 0, fmt.Errorf("FrontTrimVorbis: the first audio pages are not laid out one packet, then the rest")
	}
	// Move the second audio packet onto the first audio page.
	segs, size := 0, 0
	for segs < len(b.lacing) {
		n := int(b.lacing[segs])
		size += n
		segs++
		if n < 255 {
			break
		}
	}
	a.lacing = append(a.lacing, b.lacing[:segs]...)
	a.body = append(a.body, b.body[:size]...)
	b.lacing, b.body = b.lacing[segs:], b.body[size:]
	a.granule = held - trim
	for i := first + 1; i < len(pages); i++ {
		if pages[i].granule >= 0 {
			pages[i].granule -= trim
		}
	}
	var out bytes.Buffer
	for _, p := range pages {
		out.Write(p.bytes())
	}
	return out.Bytes(), trim, nil
}

// oggPage is one Ogg page, parsed far enough to move packets between pages and
// restate a granule.
type oggPage struct {
	head    [27]byte // as read; the granule and checksum are restated on write
	granule int64
	lacing  []byte
	body    []byte
}

// packets is how many packets complete on the page.
func (p *oggPage) packets() int {
	n := 0
	for _, l := range p.lacing {
		if l < 255 {
			n++
		}
	}
	return n
}

// continued reports whether the page opens with the rest of a packet begun on
// the page before.
func (p *oggPage) continued() bool { return p.head[5]&0x01 != 0 }

// bytes serializes the page with its granule, segment count and checksum
// restated.
func (p *oggPage) bytes() []byte {
	out := make([]byte, 0, 27+len(p.lacing)+len(p.body))
	out = append(out, p.head[:]...)
	binary.LittleEndian.PutUint64(out[6:], uint64(p.granule))
	out[26] = byte(len(p.lacing))
	clear(out[22:26])
	out = append(out, p.lacing...)
	out = append(out, p.body...)
	binary.LittleEndian.PutUint32(out[22:], oggCRC(out))
	return out
}

func oggPages(b []byte) ([]oggPage, error) {
	var pages []oggPage
	for off := 0; off < len(b); {
		if len(b)-off < 27 || string(b[off:off+4]) != "OggS" {
			return nil, fmt.Errorf("no Ogg page at offset %d", off)
		}
		var p oggPage
		copy(p.head[:], b[off:])
		p.granule = int64(binary.LittleEndian.Uint64(p.head[6:]))
		segs := int(p.head[26])
		if len(b)-off < 27+segs {
			return nil, fmt.Errorf("the page at offset %d is cut short", off)
		}
		p.lacing = bytes.Clone(b[off+27 : off+27+segs])
		size := 0
		for _, l := range p.lacing {
			size += int(l)
		}
		if len(b)-off < 27+segs+size {
			return nil, fmt.Errorf("the page at offset %d is cut short", off)
		}
		p.body = bytes.Clone(b[off+27+segs : off+27+segs+size])
		pages = append(pages, p)
		off += 27 + segs + size
	}
	return pages, nil
}

// oggCRC is Ogg's page checksum: CRC-32 with polynomial 0x04c11db7, no
// reflection, a zero start and no final XOR, over the page with its checksum
// field zeroed.
func oggCRC(page []byte) uint32 {
	var crc uint32
	for _, c := range page {
		crc ^= uint32(c) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
