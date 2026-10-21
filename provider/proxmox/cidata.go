package proxmox

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
	"time"
	"unicode/utf16"
)

// A cloud-init NoCloud seed, as the ISO a guest mounts.
//
// Proxmox cannot be handed a user-data file through its API. `cicustom` reads
// from a storage's snippets, and the upload endpoint takes only iso, vztmpl and
// import content — on Proxmox VE 9.1.6 it answered a snippets upload with
// "value 'snippets' does not have a value in the enumeration 'iso, vztmpl,
// import'". Everything that writes snippets does it over SSH, and Sybil holding
// root-capable SSH credentials for a hypervisor is the thing it does not do.
//
// An ISO is content the API does accept, and a NoCloud seed is just an ISO
// labelled "cidata" carrying user-data and meta-data at its root. cloud-init
// finds it by that label. So the seed is built here and uploaded as iso, and
// the clone's cloud-init drive is swapped for it.
//
// The format is written by hand rather than pulled in as a dependency: three
// small files in one directory is a few hundred bytes of descriptors, and the
// layout below is the whole of it. Two directory trees point at the same file
// extents — the ISO 9660 one, whose names are upper-case 8.3, and a Joliet one
// carrying the real names. Linux mounts the Joliet tree when it is present,
// which is how "user-data" is read back as "user-data".

const ISOSector = 2048

// CidataLabel is the volume label cloud-init's NoCloud datasource looks for.
const CidataLabel = "cidata"

type isoFile struct {
	name  string // the Joliet name, as the guest sees it
	short string // the ISO 9660 level 1 name, without ";1"
	data  []byte
	lba   uint32
}

// BuildNoCloudISO returns a NoCloud seed holding files at its root. Names must
// be unique; the ISO 9660 short names are derived and must not collide.
func BuildNoCloudISO(files map[string][]byte, when time.Time) ([]byte, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("cidata: no files")
	}
	list := make([]*isoFile, 0, len(files))
	seen := map[string]string{}
	for name, data := range files {
		short := shortISOName(name)
		if prev, dup := seen[short]; dup {
			return nil, fmt.Errorf("cidata: %q and %q have the same ISO 9660 name %s", prev, name, short)
		}
		seen[short] = name
		list = append(list, &isoFile{name: name, short: short, data: data})
	}
	// Directory records must be sorted, and each tree by its own names.
	sort.Slice(list, func(i, j int) bool { return list[i].short < list[j].short })
	joliet := append([]*isoFile(nil), list...)
	sort.Slice(joliet, func(i, j int) bool {
		return bytes.Compare(UCS2(joliet[i].name), UCS2(joliet[j].name)) < 0
	})

	// Layout, in sectors:
	//   0-15 system area · 16 primary descriptor · 17 Joliet descriptor ·
	//   18 terminator · 19-22 path tables (L, M, Joliet L, Joliet M) ·
	//   23 root directory · 24 Joliet root directory · 25… file data
	const (
		pvdLBA, svdLBA, termLBA                = 16, 17, 18
		pathL, pathM, jPathL, jPathM           = 19, 20, 21, 22
		rootLBA, jRootLBA, firstFileLBA uint32 = 23, 24, 25
	)
	next := firstFileLBA
	for _, f := range list {
		f.lba = next
		next += sectorsFor(len(f.data))
	}
	total := next

	stamp := dirDate(when)
	rootDir := directory(rootLBA, list, stamp, func(f *isoFile) []byte { return []byte(f.short + ";1") })
	jRootDir := directory(jRootLBA, joliet, stamp, func(f *isoFile) []byte { return UCS2(f.name) })
	if len(rootDir) > ISOSector || len(jRootDir) > ISOSector {
		return nil, fmt.Errorf("cidata: too many files for a one-sector directory")
	}

	img := make([]byte, int(total)*ISOSector)
	put := func(lba uint32, b []byte) { copy(img[int(lba)*ISOSector:], b) }

	put(pvdLBA, volumeDescriptor(1, total, rootLBA, pathL, pathM, when, false))
	put(svdLBA, volumeDescriptor(2, total, jRootLBA, jPathL, jPathM, when, true))
	put(termLBA, append([]byte{255}, []byte("CD001\x01")...))
	put(pathL, pathTable(rootLBA, binary.LittleEndian))
	put(pathM, pathTable(rootLBA, binary.BigEndian))
	put(jPathL, pathTable(jRootLBA, binary.LittleEndian))
	put(jPathM, pathTable(jRootLBA, binary.BigEndian))
	put(rootLBA, rootDir)
	put(jRootLBA, jRootDir)
	for _, f := range list {
		put(f.lba, f.data)
	}
	return img, nil
}

func sectorsFor(n int) uint32 {
	if n == 0 {
		return 0
	}
	return uint32((n + ISOSector - 1) / ISOSector)
}

// shortISOName maps a name onto ISO 9660 level 1 d-characters: upper case,
// A-Z 0-9 and underscore, at most eight characters and a dot. Nothing reads
// these names when the Joliet tree is there; they exist because the primary
// tree has to.
func shortISOName(name string) string {
	var b []byte
	for _, r := range name {
		if len(b) == 8 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z':
			b = append(b, byte(r-'a'+'A'))
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_':
			b = append(b, byte(r))
		}
	}
	return string(b) + "."
}

func UCS2(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 2*len(units))
	for i, u := range units {
		binary.BigEndian.PutUint16(out[2*i:], u)
	}
	return out
}

func both16(v uint16) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint16(b, v)
	binary.BigEndian.PutUint16(b[2:], v)
	return b
}

func both32(v uint32) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b, v)
	binary.BigEndian.PutUint32(b[4:], v)
	return b
}

func dirDate(t time.Time) []byte {
	t = t.UTC()
	return []byte{byte(t.Year() - 1900), byte(t.Month()), byte(t.Day()),
		byte(t.Hour()), byte(t.Minute()), byte(t.Second()), 0}
}

func dirRecord(lba uint32, size uint32, dir bool, stamp, name []byte) []byte {
	n := 33 + len(name)
	if n%2 == 1 {
		n++
	}
	r := make([]byte, n)
	r[0] = byte(n)
	copy(r[2:], both32(lba))
	copy(r[10:], both32(size))
	copy(r[18:], stamp)
	if dir {
		r[25] = 2
	}
	copy(r[28:], both16(1))
	r[32] = byte(len(name))
	copy(r[33:], name)
	return r
}

func directory(self uint32, files []*isoFile, stamp []byte, name func(*isoFile) []byte) []byte {
	var d []byte
	d = append(d, dirRecord(self, ISOSector, true, stamp, []byte{0})...)
	d = append(d, dirRecord(self, ISOSector, true, stamp, []byte{1})...)
	for _, f := range files {
		d = append(d, dirRecord(f.lba, uint32(len(f.data)), false, stamp, name(f))...)
	}
	return d
}

// pathTable is the one-entry table of a volume with nothing but a root.
func pathTable(root uint32, order binary.ByteOrder) []byte {
	t := make([]byte, 10)
	t[0] = 1 // name length: the root's one-byte name
	order.PutUint32(t[2:], root)
	order.PutUint16(t[6:], 1) // the root is its own parent
	return t
}

func volumeDescriptor(kind byte, total, root uint32, pathL, pathM uint32, when time.Time, joliet bool) []byte {
	v := make([]byte, ISOSector)
	v[0] = kind
	copy(v[1:], "CD001")
	v[6] = 1
	text := func(off, n int, s string) {
		if joliet {
			b := UCS2(s)
			for i := 0; i+1 < n; i += 2 {
				v[off+i], v[off+i+1] = 0, ' '
			}
			copy(v[off:off+n], b)
			return
		}
		for i := 0; i < n; i++ {
			v[off+i] = ' '
		}
		copy(v[off:off+n], s)
	}
	text(8, 32, "")
	text(40, 32, CidataLabel)
	copy(v[80:], both32(total))
	if joliet {
		copy(v[88:], "%/E") // UCS-2 level 3
	}
	copy(v[120:], both16(1))
	copy(v[124:], both16(1))
	copy(v[128:], both16(ISOSector))
	copy(v[132:], both32(10))
	binary.LittleEndian.PutUint32(v[140:], pathL)
	binary.BigEndian.PutUint32(v[148:], pathM)
	copy(v[156:], dirRecord(root, ISOSector, true, dirDate(when), []byte{0}))
	for _, f := range [][2]int{{190, 128}, {318, 128}, {446, 128}, {574, 128}, {702, 37}, {739, 37}, {776, 37}} {
		text(f[0], f[1], "")
	}
	ts := []byte(when.UTC().Format("20060102150405") + "00\x00")
	zero := []byte("0000000000000000\x00")
	copy(v[813:], ts)
	copy(v[830:], ts)
	copy(v[847:], zero)
	copy(v[864:], zero)
	v[881] = 1
	return v
}
