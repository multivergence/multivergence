// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

package multivergence

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// A store file is an 8-byte header, then records. Each record is its
// payload's length (4 bytes, little-endian), the payload's CRC-32C (4
// bytes), and the payload. A payload is one change, or, as the first record
// of a compacted file, a snapshot of the whole store. docs/FORMAT.md has the
// details.
//
// The header is "MVERGE", a flags byte and the format version (1). Flag 1
// marks a compacted file, whose first record is a snapshot written to a new
// file and synced before it was renamed into place, so it can never be torn.
var (
	fileMagic    = []byte("MVERGE\x00\x01")
	compactMagic = []byte("MVERGE\x01\x01")
)

const (
	recHeaderLen = 8
	maxRecord    = 1 << 30
	headerLen    = 8
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func frame(payload []byte) []byte {
	b := make([]byte, recHeaderLen, recHeaderLen+len(payload))
	binary.LittleEndian.PutUint32(b, uint32(len(payload)))
	binary.LittleEndian.PutUint32(b[4:], crc32.Checksum(payload, castagnoli))
	return append(b, payload...)
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// frameAhead reports whether a valid record starts anywhere in b. A torn
// write can only damage the last record, so a bad record with a good one
// after it is corruption.
func frameAhead(b []byte) bool {
	for k := 0; k+recHeaderLen < len(b); k++ {
		n := binary.LittleEndian.Uint32(b[k:])
		if n == 0 || uint64(n) > uint64(len(b)-k-recHeaderLen) {
			continue
		}
		if crc32.Checksum(b[k+recHeaderLen:k+recHeaderLen+int(n)], castagnoli) == binary.LittleEndian.Uint32(b[k+4:]) {
			return true
		}
	}
	return false
}

// parseRecords splits the bytes after the header into record payloads.
//
// It stops at a torn tail, which is what a crash in the middle of a write
// can leave, and reports where the good records end. A tail is torn when it
// is all zero bytes (a file extended before its data reached the disk), or
// when the last record is cut short or fails its checksum and no valid
// record follows it. Anything else that doesn't parse is corruption, and so
// is a bad first record in a compacted file. No payload is ever empty.
func parseRecords(b []byte, compacted bool) (recs [][]byte, end int, torn bool, err error) {
	off := 0
	corrupt := func(why string) ([][]byte, int, bool, error) {
		return nil, 0, false, fmt.Errorf("%w: %s at byte %d", ErrCorrupt, why, headerLen+off)
	}
	for off < len(b) {
		rest := b[off:]
		mayTear := !(compacted && len(recs) == 0)
		if allZero(rest) && mayTear {
			return recs, off, true, nil
		}
		if len(rest) < recHeaderLen {
			if !mayTear {
				return corrupt("cut-short snapshot")
			}
			return recs, off, true, nil
		}
		n := binary.LittleEndian.Uint32(rest)
		sum := binary.LittleEndian.Uint32(rest[4:])
		if n == 0 {
			return corrupt("empty record")
		}
		if uint64(n) > uint64(len(rest)-recHeaderLen) {
			if !mayTear || n > maxRecord || frameAhead(rest[recHeaderLen:]) {
				return corrupt(fmt.Sprintf("record claiming %d bytes", n))
			}
			return recs, off, true, nil
		}
		payload := rest[recHeaderLen : recHeaderLen+int(n)]
		if crc32.Checksum(payload, castagnoli) != sum {
			if !mayTear || recHeaderLen+int(n) != len(rest) {
				return corrupt("checksum mismatch")
			}
			return recs, off, true, nil
		}
		recs = append(recs, payload)
		off += recHeaderLen + int(n)
	}
	return recs, off, false, nil
}

// loaded is a store file read into memory.
type loaded struct {
	st        *state
	events    []Event
	eventSeq  uint64
	end       int64 // where the good records end in the file
	torn      bool
	snapBytes int64 // size of the snapshot record, 0 if none
	logBytes  int64 // bytes of change records after it
}

// load replays a file's bytes. An empty file or a cut-short header is a new
// store.
func load(data []byte, histLimit int) (*loaded, error) {
	l := &loaded{st: newState()}
	if len(data) < headerLen {
		if !bytes.HasPrefix(fileMagic, data) {
			return nil, fmt.Errorf("%w: not a multivergence file", ErrCorrupt)
		}
		l.torn = len(data) > 0
		return l, nil
	}
	compacted := bytes.Equal(data[:headerLen], compactMagic)
	if !compacted && !bytes.Equal(data[:headerLen], fileMagic) {
		return nil, fmt.Errorf("%w: not a multivergence file, or a version this one can't read", ErrCorrupt)
	}
	recs, end, torn, err := parseRecords(data[headerLen:], compacted)
	if err != nil {
		return nil, err
	}
	l.end, l.torn = int64(headerLen+end), torn
	if compacted && (len(recs) == 0 || recs[0][0] != recSnapshot) {
		return nil, fmt.Errorf("%w: compacted file without a snapshot", ErrCorrupt)
	}
	for k, p := range recs {
		if p[0] == recSnapshot {
			if k != 0 {
				return nil, fmt.Errorf("%w: snapshot in the middle of the log", ErrCorrupt)
			}
			st, ev, seq, ok := decodeSnapshot(p)
			if !ok {
				return nil, fmt.Errorf("%w: bad snapshot", ErrCorrupt)
			}
			l.st, l.events, l.eventSeq = st, ev, seq
			l.snapBytes = int64(recHeaderLen + len(p))
			continue
		}
		o, ok := decodeOp(p)
		if !ok {
			return nil, fmt.Errorf("%w: bad record %d", ErrCorrupt, k)
		}
		t, a, err := apply(l.st, o, 0)
		if err != nil {
			return nil, fmt.Errorf("%w: record %d no longer applies: %v", ErrCorrupt, k, err)
		}
		l.st = t
		l.events, l.eventSeq = addEvents(l.events, l.eventSeq, a.events, o.now, t.gen, histLimit)
		l.logBytes += int64(recHeaderLen + len(p))
	}
	return l, nil
}

// addEvents numbers new events and appends them. The history keeps up to
// limit events; it is trimmed back to limit only when it reaches twice that,
// so trimming costs nothing per change on average.
func addEvents(events []Event, seq uint64, drafts []eventDraft, now int64, gen uint64, limit int) ([]Event, uint64) {
	for _, d := range drafts {
		seq++
		g := gen
		if d.before {
			g--
		}
		events = append(events, Event{Seq: seq, Time: unixUTC(now), Generation: g, Kind: d.kind, Branch: d.branch, Detail: d.detail})
	}
	if len(events) >= 2*limit {
		events = append([]Event(nil), events[len(events)-limit:]...)
	}
	return events, seq
}

// lastEvents returns at most limit of the newest events.
func lastEvents(events []Event, limit int) []Event {
	if len(events) > limit {
		events = events[len(events)-limit:]
	}
	return append([]Event(nil), events...)
}

// logFile is the open file of a writable store.
type logFile struct {
	path      string
	f         *os.File
	lock      *os.File
	end       int64
	logBytes  int64
	snapBytes int64
	noSync    bool
	retryAt   int64 // after a failed compaction, the log size to try again at
}

func openLog(path string, opts *Options) (*logFile, *loaded, error) {
	lock, err := lockFile(path+".lock", opts.lockTimeout())
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (*logFile, *loaded, error) {
		unlockFile(lock)
		return nil, nil, err
	}
	// A compaction cut off by a crash leaves its unfinished file behind.
	if err := os.Remove(path + ".compact"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return fail(err)
	}
	failf := func(err error) (*logFile, *loaded, error) {
		f.Close()
		return fail(err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return failf(err)
	}
	ld, err := load(data, opts.historyLimit())
	if err != nil {
		return failf(err)
	}
	lf := &logFile{path: path, f: f, lock: lock, end: ld.end, logBytes: ld.logBytes, snapBytes: ld.snapBytes, noSync: opts != nil && opts.NoSync}
	if len(data) < headerLen {
		// New file, or a header cut short when it was being created.
		if err := f.Truncate(0); err != nil {
			return failf(err)
		}
		if _, err := f.WriteAt(fileMagic, 0); err != nil {
			return failf(err)
		}
		if err := f.Sync(); err != nil {
			return failf(err)
		}
		syncDir(filepath.Dir(path))
		lf.end = headerLen
	} else if ld.torn {
		// Keep the bytes being cut off, unless they are only zeros: the
		// record they started never completed, but nothing is thrown away.
		if tail := data[ld.end:]; !allZero(tail) {
			name := path + ".torn-" + strconv.FormatInt(time.Now().UnixNano(), 10)
			if err := os.WriteFile(name, tail, 0o644); err != nil {
				return failf(fmt.Errorf("multivergence: saving the torn tail to %s: %w", name, err))
			}
		}
		if err := f.Truncate(ld.end); err != nil {
			return failf(err)
		}
		if err := f.Sync(); err != nil {
			return failf(err)
		}
	}
	return lf, ld, nil
}

// append writes one record at the end and syncs it. If either fails, it
// tries to cut the file back, so the record that failed can't stay behind.
func (l *logFile) append(payload []byte) error {
	rec := frame(payload)
	_, err := l.f.WriteAt(rec, l.end)
	if err == nil && !l.noSync {
		err = l.f.Sync()
	}
	if err != nil {
		if terr := l.f.Truncate(l.end); terr != nil {
			return fmt.Errorf("%w (and cutting the record back off failed: %v)", err, terr)
		}
		l.f.Sync()
		return err
	}
	l.end += int64(len(rec))
	l.logBytes += int64(len(rec))
	return nil
}

// due reports whether the log has grown enough to compact.
func (l *logFile) due(limit int64) bool {
	return l.logBytes > limit && l.logBytes > l.retryAt
}

// compact writes the whole store as one snapshot to a new file and renames
// it over the old one. A crash before the rename leaves the old file, and
// after it the new one; both hold the same store. If writing or renaming
// fails (on Windows, the rename fails while a reader has the file open),
// the old file stays in use and the next try waits until the log has
// doubled. It returns an error only if the store's file can't be reopened.
func (l *logFile) compact(snapshot []byte) error {
	tmp := l.path + ".compact"
	rec := frame(snapshot)
	skip := func() error {
		os.Remove(tmp)
		l.retryAt = 2 * l.logBytes
		return nil
	}
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return skip()
	}
	_, err = f.Write(append(append([]byte(nil), compactMagic...), rec...))
	if err == nil && !l.noSync {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return skip()
	}
	// Windows can't rename over a file this process has open.
	if err := l.f.Close(); err != nil {
		skip()
		return l.reopen()
	}
	if err := os.Rename(tmp, l.path); err != nil {
		skip()
		return l.reopen()
	}
	if !l.noSync {
		syncDir(filepath.Dir(l.path))
	}
	if err := l.reopen(); err != nil {
		return err
	}
	l.snapBytes = int64(len(rec))
	l.logBytes = 0
	l.retryAt = 0
	return nil
}

func (l *logFile) reopen() error {
	f, err := os.OpenFile(l.path, os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.end = f, fi.Size()
	return nil
}

func (l *logFile) close() error {
	err := l.f.Close()
	unlockFile(l.lock)
	return err
}
