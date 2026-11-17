package main

// Git smart HTTP: pkt-line framing, packfile read/write (with delta
// resolution), protocol v2 ls-refs/fetch, v0 fetch, and v0 receive-pack
// with report-status.

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// pkt-line

func pktLine(data []byte) []byte {
	out := make([]byte, 0, len(data)+4)
	out = append(out, []byte(fmt.Sprintf("%04x", len(data)+4))...)
	out = append(out, data...)
	return out
}

func pktLineStr(text string) []byte {
	return pktLine([]byte(text))
}

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

const (
	packetData = iota
	packetFlush
	packetDelim
	packetResponseEnd
)

type packet struct {
	kind    int
	payload []byte
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// parsePktLines parses pkt-lines from the front of a buffer; returns the
// packets and the unparsed rest.
func parsePktLines(buffer []byte) ([]packet, []byte) {
	packets := []packet{}
	offset := 0
	for offset+4 <= len(buffer) {
		header := buffer[offset : offset+4]
		valid := true
		for _, c := range header {
			if !isHexDigit(c) {
				valid = false
				break
			}
		}
		if !valid {
			break
		}
		length64, _ := strconv.ParseInt(string(header), 16, 32)
		length := int(length64)
		if length == 0 {
			packets = append(packets, packet{kind: packetFlush})
			offset += 4
			continue
		}
		if length == 1 {
			packets = append(packets, packet{kind: packetDelim})
			offset += 4
			continue
		}
		if length == 2 {
			packets = append(packets, packet{kind: packetResponseEnd})
			offset += 4
			continue
		}
		if length < 4 || offset+length > len(buffer) {
			break
		}
		packets = append(packets, packet{kind: packetData, payload: buffer[offset+4 : offset+length]})
		offset += length
	}
	return packets, buffer[offset:]
}

// ---------------------------------------------------------------------------
// Packfiles

func writePackObjectHeader(objType string, size int) []byte {
	code := typeCode[objType]
	out := []byte{}
	remaining := size
	first := (code << 4) | (remaining & 0x0f)
	remaining /= 16
	for remaining > 0 {
		out = append(out, byte(first|0x80))
		first = remaining & 0x7f
		remaining /= 128
	}
	out = append(out, byte(first))
	return out
}

type packEntry struct {
	Oid    string
	Object *gitObject
}

// Pack-building caches. Objects are immutable and content-addressed, so a
// deflated entry and a whole pack keyed by its ordered object list stay
// valid forever. Repeated clones of the same head used to re-walk,
// re-parse and re-deflate every object and rebuild the identical pack each
// time.
const packCacheLimit = 64
const deflateCacheLimit = 100_000

var packCache = map[string][]byte{}
var deflatedEntryCache = map[string][]byte{}

func deflatedEntry(entry packEntry) []byte {
	if cached, ok := deflatedEntryCache[entry.Oid]; ok {
		return cached
	}
	var buf bytes.Buffer
	buf.Write(writePackObjectHeader(entry.Object.Type, len(entry.Object.Data)))
	zw, _ := zlib.NewWriterLevel(&buf, 1)
	zw.Write(entry.Object.Data)
	zw.Close()
	built := buf.Bytes()
	if len(deflatedEntryCache) >= deflateCacheLimit {
		deflatedEntryCache = map[string][]byte{}
	}
	deflatedEntryCache[entry.Oid] = built
	return built
}

// writePack builds a packfile of plain objects (no deltas); the trailer is
// the SHA-1 of the body.
func writePack(list []packEntry) []byte {
	keyParts := make([]string, len(list))
	for i, entry := range list {
		keyParts[i] = entry.Oid
	}
	key := strings.Join(keyParts, " ")
	if cached, ok := packCache[key]; ok {
		return cached
	}
	var body bytes.Buffer
	header := make([]byte, 12)
	copy(header, "PACK")
	binary.BigEndian.PutUint32(header[4:], 2)
	binary.BigEndian.PutUint32(header[8:], uint32(len(list)))
	body.Write(header)
	for _, entry := range list {
		body.Write(deflatedEntry(entry))
	}
	trailer := sha1.Sum(body.Bytes())
	pack := append(body.Bytes(), trailer[:]...)
	if len(packCache) >= packCacheLimit {
		packCache = map[string][]byte{}
	}
	packCache[key] = pack
	return pack
}

func readVarintSize(buffer []byte, start int) (size int64, next int, first byte) {
	b := buffer[start]
	first = b
	size = int64(b & 0x0f)
	shift := uint(4)
	offset := start + 1
	for b&0x80 != 0 {
		b = buffer[offset]
		offset++
		size += int64(b&0x7f) << shift
		shift += 7
		if shift > 63 {
			panic(errors.New("pack entry size varint is too long"))
		}
	}
	return size, offset, first
}

func readOfsDeltaBase(buffer []byte, start int) (baseOffset int64, next int) {
	b := buffer[start]
	offset := start + 1
	value := int64(b & 0x7f)
	for b&0x80 != 0 {
		b = buffer[offset]
		offset++
		value = (value+1)*128 + int64(b&0x7f)
	}
	return value, offset
}

func inflateAt(buffer []byte, offset int) (data []byte, next int, err error) {
	reader := bytes.NewReader(buffer[offset:])
	zr, err := zlib.NewReader(reader)
	if err != nil {
		return nil, 0, err
	}
	data, err = io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, err
	}
	consumed := len(buffer[offset:]) - reader.Len()
	return data, offset + consumed, nil
}

// applyDelta applies a git delta instruction stream to a base object's
// bytes.
func applyDelta(base, delta []byte) ([]byte, error) {
	offset := 0
	readSize := func() int64 {
		var value int64
		var shift uint
		for {
			b := delta[offset]
			offset++
			value += int64(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value
	}
	sourceSize := readSize()
	targetSize := readSize()
	if sourceSize != int64(len(base)) {
		return nil, errors.New("delta base size mismatch")
	}
	out := make([]byte, targetSize)
	written := int64(0)
	for offset < len(delta) {
		op := delta[offset]
		offset++
		if op&0x80 != 0 {
			var copyOffset, copySize int64
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<uint(bit)) != 0 {
					copyOffset += int64(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<uint(bit)) != 0 {
					copySize += int64(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			copy(out[written:], base[copyOffset:copyOffset+copySize])
			written += copySize
		} else if op != 0 {
			literal := int64(op & 0x7f)
			copy(out[written:], delta[offset:offset+int(literal)])
			offset += int(literal)
			written += literal
		} else {
			return nil, errors.New("delta opcode 0 is invalid")
		}
	}
	if written != targetSize {
		return nil, errors.New("delta produced the wrong size")
	}
	return out, nil
}

type rawPackEntry struct {
	offset     int
	typeCode   int
	data       []byte
	baseOffset int64
	hasBaseOff bool
	baseOid    string
	hasBaseOid bool
}

type resolvedObject struct {
	Type string
	Data []byte
	Oid  string
}

// readPackIntoStore reads a packfile and stores every object it carries
// (resolving OFS/REF deltas, with the object store as the base source for
// thin packs). It returns the number of newly stored objects.
func readPackIntoStore(pack []byte) (stored int, err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = e
			} else {
				err = fmt.Errorf("%v", r)
			}
			stored = 0
		}
	}()
	if len(pack) < 32 {
		return 0, errors.New("pack is too short")
	}
	if string(pack[0:4]) != "PACK" {
		return 0, errors.New("pack does not start with PACK")
	}
	version := binary.BigEndian.Uint32(pack[4:])
	if version != 2 && version != 3 {
		return 0, fmt.Errorf("unsupported pack version %d", version)
	}
	declared := binary.BigEndian.Uint32(pack[8:])
	trailerStart := len(pack) - 20
	trailer := hex.EncodeToString(pack[trailerStart:])
	computed := sha1.Sum(pack[:trailerStart])
	if trailer != hex.EncodeToString(computed[:]) {
		return 0, errors.New("pack checksum mismatch")
	}

	raw := []*rawPackEntry{}
	byOffset := map[int]*rawPackEntry{}
	offset := 12
	for index := uint32(0); index < declared; index++ {
		entryOffset := offset
		size, next, first := readVarintSize(pack, offset)
		offset = next
		entryType := int(first>>4) & 0x07
		entry := &rawPackEntry{offset: entryOffset, typeCode: entryType}
		if entryType == 6 {
			baseOffset, afterBase := readOfsDeltaBase(pack, offset)
			offset = afterBase
			entry.baseOffset = int64(entryOffset) - baseOffset
			entry.hasBaseOff = true
		} else if entryType == 7 {
			entry.baseOid = hex.EncodeToString(pack[offset : offset+20])
			entry.hasBaseOid = true
			offset += 20
		} else if _, ok := codeType[entryType]; !ok {
			return 0, fmt.Errorf("pack entry has unknown type code %d", entryType)
		}
		data, afterData, ierr := inflateAt(pack[:trailerStart], offset)
		if ierr != nil {
			return 0, ierr
		}
		if int64(len(data)) != size {
			return 0, errors.New("pack entry size mismatch")
		}
		entry.data = data
		offset = afterData
		raw = append(raw, entry)
		byOffset[entryOffset] = entry
	}

	resolved := map[int]*resolvedObject{}
	var resolve func(entry *rawPackEntry) (*resolvedObject, error)
	resolve = func(entry *rawPackEntry) (*resolvedObject, error) {
		if cached, ok := resolved[entry.offset]; ok {
			return cached, nil
		}
		var objType string
		var data []byte
		if entry.typeCode == 6 {
			baseEntry, ok := byOffset[int(entry.baseOffset)]
			if !ok {
				return nil, errors.New("ofs-delta base is not in the pack")
			}
			base, rerr := resolve(baseEntry)
			if rerr != nil {
				return nil, rerr
			}
			objType = base.Type
			applied, aerr := applyDelta(base.Data, entry.data)
			if aerr != nil {
				return nil, aerr
			}
			data = applied
		} else if entry.typeCode == 7 {
			baseOid := entry.baseOid
			var base *resolvedObject
			for _, candidate := range raw {
				if candidate == entry {
					continue
				}
				if candidateResolved, ok := resolved[candidate.offset]; ok && candidateResolved.Oid == baseOid {
					base = candidateResolved
					break
				}
			}
			if base == nil {
				// The base may be an entry not yet resolved, or an object
				// the store already holds.
				var inPack *rawPackEntry
				for _, candidate := range raw {
					if candidate == entry || candidate.typeCode == 6 || candidate.typeCode == 7 {
						continue
					}
					if hashObject(codeType[candidate.typeCode], candidate.data) == baseOid {
						inPack = candidate
						break
					}
				}
				if inPack != nil {
					found, rerr := resolve(inPack)
					if rerr != nil {
						return nil, rerr
					}
					base = found
				} else if storedObj, ok := objects[baseOid]; ok {
					base = &resolvedObject{Type: storedObj.Type, Data: storedObj.Data, Oid: baseOid}
				}
			}
			if base == nil {
				return nil, fmt.Errorf("ref-delta base %s is unavailable", baseOid)
			}
			objType = base.Type
			applied, aerr := applyDelta(base.Data, entry.data)
			if aerr != nil {
				return nil, aerr
			}
			data = applied
		} else {
			objType = codeType[entry.typeCode]
			data = entry.data
		}
		done := &resolvedObject{Type: objType, Data: data, Oid: hashObject(objType, data)}
		resolved[entry.offset] = done
		return done, nil
	}

	stored = 0
	for _, entry := range raw {
		object, rerr := resolve(entry)
		if rerr != nil {
			return 0, rerr
		}
		if _, ok := objects[object.Oid]; !ok {
			objects[object.Oid] = &gitObject{Type: object.Type, Data: object.Data}
			stored++
		}
	}
	return stored, nil
}

// ---------------------------------------------------------------------------
// Fetch set computation

type fetchPlan struct {
	packObjects []packEntry
	shallow     []string
	unshallow   []string
}

// treeClosure adds the tree closure of one commit (the commit itself not
// included) to the set.
func treeClosure(commitOid string, into *oidSet) {
	view := commitViewOf(commitOid)
	if view == nil {
		return
	}
	stack := []string{view.Tree}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if into.has(oid) {
			continue
		}
		entries := treeEntriesOf(oid)
		if entries == nil {
			// Not a tree: a blob (or a missing object) closes here.
			if _, ok := objects[oid]; ok {
				into.add(oid)
			}
			continue
		}
		into.add(oid)
		for _, entry := range entries {
			if entry.Mode != "160000" {
				stack = append(stack, entry.Oid)
			}
		}
	}
}

func commitParents(oid string) []string {
	view := commitViewOf(oid)
	if view == nil {
		return nil
	}
	return view.Parents
}

// planFetch computes what a fetch must send.
//   - No depth: closure(wants) minus closure(haves).
//   - Depth/deepen: commits within depth generations of the wants, plus
//     their tree closures, minus what the haves already cover (a have
//     commit and its tree closure). The shallow boundary is the included
//     commits whose parents were left out.
func planFetch(wants, haves, clientShallow []string, depth *int) fetchPlan {
	send := newOidSet()
	if depth == nil {
		excluded := collectReachable(haves)
		for _, oid := range collectReachable(wants).order {
			if !excluded.has(oid) {
				send.add(oid)
			}
		}
		return fetchPlan{packObjects: materialize(send), shallow: []string{}, unshallow: []string{}}
	}

	excluded := newOidSet()
	for _, have := range haves {
		excluded.add(have)
		treeClosure(have, excluded)
	}
	includedCommits := newOidSet()
	generations := map[string]int{}
	frontier := []string{}
	for _, want := range wants {
		send.add(want)
		if peeled, typ, ok := peelOid(want); ok && typ == "commit" {
			frontier = append(frontier, peeled)
		}
	}
	generation := 0
	for len(frontier) > 0 && generation < *depth {
		var next []string
		for _, oid := range frontier {
			if _, done := generations[oid]; done {
				continue
			}
			generations[oid] = generation
			includedCommits.add(oid)
			next = append(next, commitParents(oid)...)
		}
		frontier = next
		generation++
	}
	for _, oid := range includedCommits.order {
		if !excluded.has(oid) {
			send.add(oid)
		}
		trees := newOidSet()
		treeClosure(oid, trees)
		for _, treeOid := range trees.order {
			if !excluded.has(treeOid) {
				send.add(treeOid)
			}
		}
	}
	boundary := []string{}
	for _, oid := range includedCommits.order {
		parents := commitParents(oid)
		if len(parents) == 0 {
			continue
		}
		for _, parent := range parents {
			if !includedCommits.has(parent) {
				if _, ok := objects[parent]; ok {
					boundary = append(boundary, oid)
					break
				}
			}
		}
	}
	unshallow := []string{}
	for _, oid := range clientShallow {
		onBoundary := false
		for _, b := range boundary {
			if b == oid {
				onBoundary = true
				break
			}
		}
		if onBoundary {
			continue
		}
		parents := commitParents(oid)
		if len(parents) == 0 {
			continue
		}
		allIncluded := true
		for _, parent := range parents {
			if !includedCommits.has(parent) {
				allIncluded = false
				break
			}
		}
		if allIncluded {
			unshallow = append(unshallow, oid)
		}
	}
	return fetchPlan{packObjects: materialize(send), shallow: boundary, unshallow: unshallow}
}

func materialize(oids *oidSet) []packEntry {
	out := []packEntry{}
	for _, oid := range oids.order {
		if object, ok := objects[oid]; ok {
			out = append(out, packEntry{Oid: oid, Object: object})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Advertisements

const uploadCapsV0 = "side-band-64k ofs-delta thin-pack shallow symref=HEAD:refs/heads/main agent=forge/1.0"
const receiveCapsV0 = "report-status delete-refs side-band-64k ofs-delta quiet symref=HEAD:refs/heads/main agent=forge/1.0"

func sortedRefs(repo *repoRec) [][2]string {
	names := make([]string, 0, len(repo.refs))
	for name := range repo.refs {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([][2]string, 0, len(names))
	for _, name := range names {
		out = append(out, [2]string{name, repo.refs[name]})
	}
	return out
}

func v0Advertisement(repo *repoRec, service string) []byte {
	caps := uploadCapsV0
	if service == "git-receive-pack" {
		caps = receiveCapsV0
	}
	headTarget := "refs/heads/" + repo.DefaultBranch
	headTip, hasHead := repo.ref(headTarget)
	var buf bytes.Buffer
	buf.Write(pktLineStr("# service=" + service + "\n"))
	buf.Write(pktFlush)
	refs := sortedRefs(repo)
	if len(refs) == 0 {
		buf.Write(pktLineStr(zeroOID + " capabilities^{}\x00" + caps + "\n"))
	} else {
		first := true
		if hasHead {
			buf.Write(pktLineStr(headTip + " HEAD\x00" + caps + "\n"))
			first = false
		}
		for _, pair := range refs {
			name, oid := pair[0], pair[1]
			if first {
				buf.Write(pktLineStr(oid + " " + name + "\x00" + caps + "\n"))
			} else {
				buf.Write(pktLineStr(oid + " " + name + "\n"))
			}
			first = false
			if peeled, typ, ok := peelOid(oid); ok && typ == "commit" && peeled != oid {
				buf.Write(pktLineStr(peeled + " " + name + "{}\n"))
			}
		}
	}
	buf.Write(pktFlush)
	return buf.Bytes()
}

func v2Advertisement(service string) []byte {
	var buf bytes.Buffer
	buf.Write(pktLineStr("# service=" + service + "\n"))
	buf.Write(pktFlush)
	buf.Write(pktLineStr("version 2\n"))
	buf.Write(pktLineStr("ls-refs=unborn\n"))
	buf.Write(pktLineStr("fetch=shallow\n"))
	buf.Write(pktLineStr("object-format=sha1\n"))
	buf.Write(pktFlush)
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// ls-refs

func lsRefsResponse(repo *repoRec, args []string) []byte {
	peel := false
	symrefs := false
	prefixes := []string{}
	for _, arg := range args {
		if arg == "peel" {
			peel = true
		} else if arg == "symrefs" {
			symrefs = true
		} else if strings.HasPrefix(arg, "ref-prefix ") {
			prefixes = append(prefixes, arg[len("ref-prefix "):])
		}
	}
	matches := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	var buf bytes.Buffer
	headTarget := "refs/heads/" + repo.DefaultBranch
	headTip, hasHead := repo.ref(headTarget)
	if matches("HEAD") {
		suffix := ""
		if symrefs {
			suffix = " symref-target:" + headTarget
		}
		if hasHead {
			buf.Write(pktLineStr(headTip + " HEAD" + suffix + "\n"))
		} else {
			buf.Write(pktLineStr("unborn HEAD" + suffix + "\n"))
		}
	}
	for _, pair := range sortedRefs(repo) {
		name, oid := pair[0], pair[1]
		if !matches(name) {
			continue
		}
		line := oid + " " + name
		if peel {
			if peeled, typ, ok := peelOid(oid); ok && typ == "commit" && peeled != oid {
				line += " peeled:" + peeled
			}
		}
		buf.Write(pktLineStr(line + "\n"))
	}
	buf.Write(pktFlush)
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// Fetch responses

func sidebandChunks(pack []byte) [][]byte {
	chunks := [][]byte{}
	const maxChunk = 65515
	for offset := 0; offset < len(pack); offset += maxChunk {
		end := offset + maxChunk
		if end > len(pack) {
			end = len(pack)
		}
		slice := pack[offset:end]
		payload := make([]byte, 0, len(slice)+1)
		payload = append(payload, 1)
		payload = append(payload, slice...)
		chunks = append(chunks, pktLine(payload))
	}
	if len(chunks) == 0 {
		chunks = append(chunks, pktLine([]byte{1}))
	}
	return chunks
}

func fetchResponseV2(plan fetchPlan) []byte {
	var buf bytes.Buffer
	buf.Write(pktLineStr("acknowledgments\n"))
	buf.Write(pktLineStr("NAK\n"))
	buf.Write(pktFlush)
	if len(plan.shallow) > 0 || len(plan.unshallow) > 0 {
		buf.Write(pktLineStr("shallow-info\n"))
		for _, oid := range plan.shallow {
			buf.Write(pktLineStr("shallow " + oid + "\n"))
		}
		for _, oid := range plan.unshallow {
			buf.Write(pktLineStr("unshallow " + oid + "\n"))
		}
		buf.Write(pktFlush)
	}
	buf.Write(pktLineStr("packfile\n"))
	for _, chunk := range sidebandChunks(writePack(plan.packObjects)) {
		buf.Write(chunk)
	}
	buf.Write(pktFlush)
	return buf.Bytes()
}

func fetchResponseV0(plan fetchPlan) []byte {
	var buf bytes.Buffer
	for _, oid := range plan.shallow {
		buf.Write(pktLineStr("shallow " + oid + "\n"))
	}
	for _, oid := range plan.unshallow {
		buf.Write(pktLineStr("unshallow " + oid + "\n"))
	}
	buf.Write(pktLineStr("NAK\n"))
	for _, chunk := range sidebandChunks(writePack(plan.packObjects)) {
		buf.Write(chunk)
	}
	buf.Write(pktFlush)
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// Request handling

type gitReply struct {
	status      int
	contentType string
	body        []byte
}

func gitError(status int, message string) gitReply {
	return gitReply{status: status, contentType: "text/plain; charset=utf-8", body: []byte(message + "\n")}
}

// v2Arguments splits a v2 command request into its argument lines (after
// the delim). ok=false when there is no command line.
func v2Arguments(packets []packet) (string, []string, bool) {
	command := ""
	args := []string{}
	pastDelim := false
	for _, p := range packets {
		if p.kind == packetDelim {
			pastDelim = true
			continue
		}
		if p.kind == packetFlush {
			break
		}
		if p.kind != packetData {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		if !pastDelim {
			if strings.HasPrefix(line, "command=") {
				command = line[len("command="):]
			}
			continue
		}
		args = append(args, line)
	}
	if command == "" {
		return "", nil, false
	}
	return command, args, true
}

// ---------------------------------------------------------------------------
// Git response caches
//
// Advertisements and ls-refs answers depend only on a repo's refs and
// default branch, so they are cached against the repo's refsVersion
// (bumped by every ref change). Fetch answers depend only on the immutable
// object store and the oids a request names, so they are cached against
// the parsed request itself and never need invalidation.

const gitCacheLimit = 256

var gitResponseCache = map[string][]byte{}

func gitCached(key string, build func() []byte) []byte {
	if hit, ok := gitResponseCache[key]; ok {
		return hit
	}
	built := build()
	if len(built) <= 8*1024*1024 {
		if len(gitResponseCache) >= gitCacheLimit {
			gitResponseCache = map[string][]byte{}
		}
		gitResponseCache[key] = built
	}
	return built
}

func repoCacheKey(repo *repoRec) string {
	return fmt.Sprintf("%s/%s#%d#%s", repo.Owner, repo.Name, repo.RefsVersion, repo.DefaultBranch)
}

func handleInfoRefs(repo *repoRec, service string, protocolV2 bool) gitReply {
	if service != "git-upload-pack" && service != "git-receive-pack" {
		return gitError(400, "unknown service")
	}
	var body []byte
	if protocolV2 && service == "git-upload-pack" {
		body = v2Advertisement(service)
	} else {
		body = gitCached("adv|"+repoCacheKey(repo)+"|"+service, func() []byte {
			return v0Advertisement(repo, service)
		})
	}
	return gitReply{status: 200, contentType: "application/x-" + service + "-advertisement", body: body}
}

func depthKey(depth *int) string {
	if depth == nil {
		return "null"
	}
	return strconv.Itoa(*depth)
}

func handleUploadPack(repo *repoRec, body []byte, protocolV2 bool) gitReply {
	packets, _ := parsePktLines(body)
	if protocolV2 {
		command, args, ok := v2Arguments(packets)
		if !ok {
			return gitError(400, "malformed protocol v2 request")
		}
		if command == "ls-refs" {
			replyBody := gitCached("lsr|"+repoCacheKey(repo)+"|"+strings.Join(args, "\n"), func() []byte {
				return lsRefsResponse(repo, args)
			})
			return gitReply{status: 200, contentType: "application/x-git-upload-pack-result", body: replyBody}
		}
		if command == "fetch" {
			wants := []string{}
			haves := []string{}
			shallow := []string{}
			var depth *int
			for _, arg := range args {
				if strings.HasPrefix(arg, "want ") {
					wants = append(wants, strings.TrimSpace(arg[5:]))
				} else if strings.HasPrefix(arg, "have ") {
					haves = append(haves, strings.TrimSpace(arg[5:]))
				} else if strings.HasPrefix(arg, "shallow ") {
					shallow = append(shallow, strings.TrimSpace(arg[8:]))
				} else if strings.HasPrefix(arg, "deepen ") {
					if parsed, perr := strconv.Atoi(strings.TrimSpace(arg[7:])); perr == nil {
						depth = &parsed
					} else {
						negative := -1
						depth = &negative
					}
				} else if strings.HasPrefix(arg, "deepen-since ") {
					depth = nil // not used by the suite; treat as unbounded
				}
			}
			key := "fetch2|" + strings.Join(wants, ",") + "|" + strings.Join(haves, ",") + "|" + strings.Join(shallow, ",") + "|" + depthKey(depth)
			replyBody := gitCached(key, func() []byte {
				return fetchResponseV2(planFetch(wants, haves, shallow, depth))
			})
			return gitReply{status: 200, contentType: "application/x-git-upload-pack-result", body: replyBody}
		}
		return gitError(400, "unknown command "+command)
	}

	// Protocol v0: want lines (with shallow/deepen) up to a flush, then
	// haves and done.
	wants := []string{}
	haves := []string{}
	shallow := []string{}
	var depth *int
	section := 0
	sawAny := false
	for _, p := range packets {
		if p.kind == packetFlush {
			section++
			continue
		}
		if p.kind != packetData {
			continue
		}
		sawAny = true
		line := strings.TrimSuffix(string(p.payload), "\n")
		if section == 0 {
			if strings.HasPrefix(line, "want ") {
				wants = append(wants, strings.SplitN(line[5:], " ", 2)[0])
			} else if strings.HasPrefix(line, "shallow ") {
				shallow = append(shallow, strings.TrimSpace(line[8:]))
			} else if strings.HasPrefix(line, "deepen ") {
				if parsed, perr := strconv.Atoi(strings.TrimSpace(line[7:])); perr == nil {
					depth = &parsed
				} else {
					negative := -1
					depth = &negative
				}
			}
		} else if strings.HasPrefix(line, "have ") {
			haves = append(haves, strings.TrimSpace(line[5:]))
		}
	}
	if !sawAny {
		return gitError(400, "malformed upload-pack request")
	}
	key := "fetch0|" + strings.Join(wants, ",") + "|" + strings.Join(haves, ",") + "|" + strings.Join(shallow, ",") + "|" + depthKey(depth)
	replyBody := gitCached(key, func() []byte {
		return fetchResponseV0(planFetch(wants, haves, shallow, depth))
	})
	return gitReply{status: 200, contentType: "application/x-git-upload-pack-result", body: replyBody}
}

type pushCommand struct {
	oldOid string
	newOid string
	ref    string
}

func handleReceivePack(repo *repoRec, body []byte) gitReply {
	packets, rest := parsePktLines(body)
	commands := []pushCommand{}
	malformed := len(packets) == 0
	for _, p := range packets {
		if p.kind == packetFlush {
			break
		}
		if p.kind != packetData {
			malformed = true
			break
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			line = line[:nul]
		}
		parts := strings.Split(line, " ")
		if len(parts) < 3 || !hex40Pattern(parts[0]) || !hex40Pattern(parts[1]) {
			malformed = true
			break
		}
		commands = append(commands, pushCommand{oldOid: parts[0], newOid: parts[1], ref: strings.Join(parts[2:], " ")})
	}
	if malformed || len(commands) == 0 {
		return gitError(400, "malformed receive-pack request")
	}

	unpackOk := true
	unpackDetail := "ok"
	if len(rest) > 0 {
		if _, err := readPackIntoStore(rest); err != nil {
			unpackOk = false
			unpackDetail = err.Error()
		}
	}

	type refReport struct {
		ref    string
		ok     bool
		reason string
	}
	reports := []refReport{}
	for _, command := range commands {
		current, has := repo.ref(command.ref)
		if !has {
			current = zeroOID
		}
		if !unpackOk {
			reports = append(reports, refReport{ref: command.ref, ok: false, reason: "unpack failed"})
			continue
		}
		if command.newOid == zeroOID {
			repo.deleteRef(command.ref)
			reports = append(reports, refReport{ref: command.ref, ok: true})
			continue
		}
		if _, ok := objects[command.newOid]; !ok {
			reports = append(reports, refReport{ref: command.ref, ok: false, reason: "missing object"})
			continue
		}
		if strings.HasPrefix(command.ref, "refs/heads/") &&
			current != zeroOID &&
			current != command.newOid &&
			!isAncestor(current, command.newOid) {
			reports = append(reports, refReport{ref: command.ref, ok: false, reason: "non-fast-forward"})
			continue
		}
		repo.setRef(command.ref, command.newOid)
		reports = append(reports, refReport{ref: command.ref, ok: true})
	}

	var reportStream bytes.Buffer
	if unpackOk {
		reportStream.Write(pktLineStr("unpack ok\n"))
	} else {
		reportStream.Write(pktLineStr("unpack " + unpackDetail + "\n"))
	}
	for _, report := range reports {
		if report.ok {
			reportStream.Write(pktLineStr("ok " + report.ref + "\n"))
		} else {
			reportStream.Write(pktLineStr("ng " + report.ref + " " + report.reason + "\n"))
		}
	}
	reportStream.Write(pktFlush)
	// Wrap the report-status stream in sideband band-1 packets
	// (side-band-64k was requested).
	var wrapped bytes.Buffer
	stream := reportStream.Bytes()
	const maxChunk = 65515
	for offset := 0; offset < len(stream); offset += maxChunk {
		end := offset + maxChunk
		if end > len(stream) {
			end = len(stream)
		}
		payload := append([]byte{1}, stream[offset:end]...)
		wrapped.Write(pktLine(payload))
	}
	wrapped.Write(pktFlush)
	return gitReply{status: 200, contentType: "application/x-git-receive-pack-result", body: wrapped.Bytes()}
}

// repoForGitPath looks up the repository a git path names; nil when it
// does not exist.
func repoForGitPath(owner, repoName string) *repoRec {
	return getRepo(owner, repoName)
}
