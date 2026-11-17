package main

// The forge's in-memory store: the git object model, repositories, issues,
// pull requests, labels and comments. Everything lives in maps for the
// lifetime of the process (one trial); the suite creates every fixture it
// needs through the public surfaces, so no seeding beyond the scenario's
// user logins (derived from the seeded RNG, ported below) is required.

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Scenario users (a faithful port of the scenario generator's Rng +
// generateUsers, so the login the token names is derived, not guessed).

const scenarioSeed uint64 = 0x6a09e667f3bcc909

func fnv1a64(text string) uint64 {
	hash := uint64(0xcbf29ce484222325)
	for i := 0; i < len(text); i++ {
		hash ^= uint64(text[i])
		hash *= 0x100000001b3
	}
	return hash
}

func mix64(value uint64) uint64 {
	z := value + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

type rng struct {
	state uint64
}

func newRng(seed uint64) *rng {
	if seed == 0 {
		seed = scenarioSeed
	}
	return &rng{state: seed}
}

func (r *rng) nextU64() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545f4914f6cdd1d
}

func (r *rng) nextU32() uint32 {
	return uint32(r.nextU64() >> 32)
}

func (r *rng) nextInt(maxExclusive int) int {
	if maxExclusive == 1 {
		return 0
	}
	rangeU := uint64(1) << 32
	limit := rangeU - (rangeU % uint64(maxExclusive))
	for {
		value := r.nextU32()
		if uint64(value) < limit {
			return int(uint64(value) % uint64(maxExclusive))
		}
	}
}

func (r *rng) pick(items []string) string {
	return items[r.nextInt(len(items))]
}

var firstNames = []string{
	"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo", "iris", "jonas", "kira", "lars",
	"mira", "nico", "olga", "pavel", "quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane",
}

var lastNames = []string{
	"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia", "holm", "iversen", "jansen", "keller",
	"lind", "moreau", "novak", "olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega", "weber",
	"xu", "yilmaz", "zimmer",
}

type scenarioUser struct {
	ID    int
	Login string
	Name  string
	Email string
}

func generateUsers(count int) []scenarioUser {
	r := newRng(mix64(scenarioSeed ^ fnv1a64("users")))
	used := map[string]bool{}
	users := make([]scenarioUser, 0, count)
	for i := 0; i < count; i++ {
		first := r.pick(firstNames)
		last := r.pick(lastNames)
		base := strings.Map(func(ch rune) rune {
			if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' {
				return ch
			}
			return -1
		}, first+last)
		login := base
		suffix := 2
		for used[login] {
			login = fmt.Sprintf("%s-%d", base, suffix)
			suffix++
		}
		used[login] = true
		users = append(users, scenarioUser{
			ID:    1000 + i,
			Login: login,
			Name:  first + " " + last,
			Email: login + "@example.invalid",
		})
	}
	return users
}

var users = generateUsers(12)
var viewer = users[0]
var stranger = users[2]

// ---------------------------------------------------------------------------
// Git object model

type gitObject struct {
	Type string // "commit" | "tree" | "blob" | "tag"
	Data []byte
}

var objects = map[string]*gitObject{}

const zeroOID = "0000000000000000000000000000000000000000"

var typeCode = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var codeType = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

func hashObject(objType string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", objType, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func storeObject(objType string, data []byte) string {
	oid := hashObject(objType, data)
	if _, ok := objects[oid]; !ok {
		objects[oid] = &gitObject{Type: objType, Data: data}
	}
	return oid
}

type commitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func parseCommit(data []byte) *commitView {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
	}
	headerText := string(data[:headerEnd])
	message := ""
	if blank >= 0 {
		message = string(data[blank+2:])
	}
	view := &commitView{}
	for _, line := range strings.Split(headerText, "\n") {
		if line == "" {
			continue
		}
		key := line
		value := ""
		if space := strings.IndexByte(line, ' '); space >= 0 {
			key = line[:space]
			value = line[space+1:]
		}
		switch key {
		case "tree":
			view.Tree = value
		case "parent":
			view.Parents = append(view.Parents, value)
		case "author":
			view.Author = value
		case "committer":
			view.Committer = value
		}
	}
	view.Message = message
	return view
}

type tagView struct {
	Object  string
	Type    string
	Tag     string
	Tagger  string
	Message string
}

func parseTag(data []byte) *tagView {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
	}
	headers := map[string]string{}
	for _, line := range strings.Split(string(data[:headerEnd]), "\n") {
		if line == "" {
			continue
		}
		space := strings.IndexByte(line, ' ')
		if space < 0 {
			continue
		}
		headers[line[:space]] = line[space+1:]
	}
	view := &tagView{
		Object: headers["object"],
		Type:   headers["type"],
		Tag:    headers["tag"],
		Tagger: headers["tagger"],
	}
	if blank >= 0 {
		view.Message = string(data[blank+2:])
	}
	return view
}

type treeEntry struct {
	Mode string
	Name string
	Oid  string
}

func parseTree(data []byte) []treeEntry {
	entries := []treeEntry{}
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], 0x20)
		if space < 0 {
			break
		}
		space += offset
		nul := bytes.IndexByte(data[space+1:], 0x00)
		if nul < 0 {
			break
		}
		nul += space + 1
		if nul+21 > len(data) {
			break
		}
		entries = append(entries, treeEntry{
			Mode: string(data[offset:space]),
			Name: string(data[space+1 : nul]),
			Oid:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		offset = nul + 21
	}
	return entries
}

// ---------------------------------------------------------------------------
// Parsed-object caches
//
// Git objects are immutable and content-addressed, so a parse keyed by oid
// stays valid forever. History walks, tree flattening and fetch planning
// used to re-parse the same commits and trees on every request; these
// caches make each object parse at most once. Callers must treat the
// returned views as read-only. The caches clear wholesale past a generous
// size cap so a long process cannot grow them without bound.

const parseCacheLimit = 100_000

var commitViewCache = map[string]*commitView{}
var tagViewCache = map[string]*tagView{}
var treeEntriesCache = map[string][]treeEntry{}

// commitViewOf returns the parsed commit at oid, or nil when it is missing
// or not a commit.
func commitViewOf(oid string) *commitView {
	if cached, ok := commitViewCache[oid]; ok {
		return cached
	}
	object, ok := objects[oid]
	if !ok || object.Type != "commit" {
		return nil
	}
	view := parseCommit(object.Data)
	if len(commitViewCache) >= parseCacheLimit {
		commitViewCache = map[string]*commitView{}
	}
	commitViewCache[oid] = view
	return view
}

// tagViewOf returns the parsed tag at oid, or nil.
func tagViewOf(oid string) *tagView {
	if cached, ok := tagViewCache[oid]; ok {
		return cached
	}
	object, ok := objects[oid]
	if !ok || object.Type != "tag" {
		return nil
	}
	view := parseTag(object.Data)
	if len(tagViewCache) >= parseCacheLimit {
		tagViewCache = map[string]*tagView{}
	}
	tagViewCache[oid] = view
	return view
}

// treeEntriesOf returns the entries of the tree at oid, or nil.
func treeEntriesOf(oid string) []treeEntry {
	if cached, ok := treeEntriesCache[oid]; ok {
		return cached
	}
	object, ok := objects[oid]
	if !ok || object.Type != "tree" {
		return nil
	}
	entries := parseTree(object.Data)
	if len(treeEntriesCache) >= parseCacheLimit {
		treeEntriesCache = map[string][]treeEntry{}
	}
	treeEntriesCache[oid] = entries
	return entries
}

func treeNameKey(entry treeEntry) string {
	if entry.Mode == "40000" {
		return entry.Name + "/"
	}
	return entry.Name
}

func buildTree(entries []treeEntry) []byte {
	sorted := make([]treeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		return treeNameKey(sorted[i]) < treeNameKey(sorted[j])
	})
	var buf bytes.Buffer
	for _, entry := range sorted {
		buf.WriteString(entry.Mode)
		buf.WriteByte(' ')
		buf.WriteString(entry.Name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(entry.Oid)
		buf.Write(raw)
	}
	return buf.Bytes()
}

func storeTree(entries []treeEntry) string {
	return storeObject("tree", buildTree(entries))
}

var emptyTreeOID = storeObject("tree", []byte{})

// peelOid follows tag objects until a non-tag object; ok=false when the
// chain breaks.
func peelOid(oid string) (string, string, bool) {
	current := oid
	for depth := 0; depth < 16; depth++ {
		object, ok := objects[current]
		if !ok {
			return "", "", false
		}
		if object.Type != "tag" {
			return current, object.Type, true
		}
		current = tagViewOf(current).Object
	}
	return "", "", false
}

func peelToCommit(oid string) (string, bool) {
	peeled, typ, ok := peelOid(oid)
	if ok && typ == "commit" {
		return peeled, true
	}
	return "", false
}

// oidSet is an insertion-ordered set of object ids, mirroring the JS Set
// the original code used: iteration follows first-insertion order.
type oidSet struct {
	set   map[string]bool
	order []string
}

func newOidSet() *oidSet {
	return &oidSet{set: map[string]bool{}}
}

func (s *oidSet) add(oid string) {
	if !s.set[oid] {
		s.set[oid] = true
		s.order = append(s.order, oid)
	}
}

func (s *oidSet) has(oid string) bool {
	return s.set[oid]
}

// collectReachable returns every object reachable from roots (commits,
// their trees and parents; tags followed), in discovery order.
func collectReachable(roots []string) *oidSet {
	seen := newOidSet()
	stack := make([]string, len(roots))
	copy(stack, roots)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen.has(oid) {
			continue
		}
		seen.add(oid)
		object, ok := objects[oid]
		if !ok {
			continue
		}
		switch object.Type {
		case "commit":
			commit := commitViewOf(oid)
			stack = append(stack, commit.Tree)
			stack = append(stack, commit.Parents...)
		case "tag":
			stack = append(stack, tagViewOf(oid).Object)
		case "tree":
			for _, entry := range treeEntriesOf(oid) {
				if entry.Mode == "160000" {
					continue
				}
				stack = append(stack, entry.Oid)
			}
		}
	}
	return seen
}

// isAncestor reports whether ancestor is reachable from descendant through
// commit parents (inclusive).
func isAncestor(ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := map[string]bool{}
	stack := []string{descendant}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		object, ok := objects[oid]
		if !ok || object.Type != "commit" {
			continue
		}
		for _, parent := range commitViewOf(oid).Parents {
			if parent == ancestor {
				return true
			}
			stack = append(stack, parent)
		}
	}
	return false
}

// ancestorDistances maps every ancestor of a commit (inclusive) to its BFS
// distance from it, in BFS discovery order.
func ancestorDistances(tip string) (map[string]int, []string) {
	distances := map[string]int{}
	order := []string{}
	frontier := []string{tip}
	distance := 0
	for len(frontier) > 0 {
		var next []string
		for _, oid := range frontier {
			if _, done := distances[oid]; done {
				continue
			}
			distances[oid] = distance
			order = append(order, oid)
			object, ok := objects[oid]
			if ok && object.Type == "commit" {
				next = append(next, commitViewOf(oid).Parents...)
			}
		}
		frontier = next
		distance++
	}
	return distances, order
}

// mergeBase finds the best common ancestor of two commits (smallest summed
// BFS distance), or "" when there is none.
func mergeBase(a, b string) string {
	if a == b {
		return a
	}
	fromA, orderA := ancestorDistances(a)
	fromB, _ := ancestorDistances(b)
	best := ""
	bestScore := int64(1) << 62
	for _, oid := range orderA {
		db, ok := fromB[oid]
		if !ok {
			continue
		}
		score := int64(fromA[oid]) + int64(db)
		if score < bestScore {
			bestScore = score
			best = oid
		}
	}
	return best
}

// ---------------------------------------------------------------------------
// Trees as flat path maps

// flattenTree flattens a tree (recursively) into path -> blob oid.
func flattenTree(treeOid string) map[string]string {
	out := map[string]string{}
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		entries := treeEntriesOf(oid)
		if entries == nil {
			return
		}
		for _, entry := range entries {
			path := entry.Name
			if prefix != "" {
				path = prefix + "/" + entry.Name
			}
			if entry.Mode == "40000" {
				walk(entry.Oid, path)
			} else {
				out[path] = entry.Oid
			}
		}
	}
	walk(treeOid, "")
	return out
}

type flatNode struct {
	files map[string]string
	dirs  map[string]*flatNode
}

func newFlatNode() *flatNode {
	return &flatNode{files: map[string]string{}, dirs: map[string]*flatNode{}}
}

// treeFromFlat builds (and stores) nested trees from a flat path -> blob
// oid map; returns the root tree oid.
func treeFromFlat(flat map[string]string) string {
	root := newFlatNode()
	for path, oid := range flat {
		segments := strings.Split(path, "/")
		node := root
		for i := 0; i < len(segments)-1; i++ {
			name := segments[i]
			child, ok := node.dirs[name]
			if !ok {
				child = newFlatNode()
				node.dirs[name] = child
			}
			node = child
		}
		node.files[segments[len(segments)-1]] = oid
	}
	var build func(node *flatNode) string
	build = func(node *flatNode) string {
		entries := []treeEntry{}
		for name, oid := range node.files {
			entries = append(entries, treeEntry{Mode: "100644", Name: name, Oid: oid})
		}
		for name, child := range node.dirs {
			entries = append(entries, treeEntry{Mode: "40000", Name: name, Oid: build(child)})
		}
		return storeTree(entries)
	}
	return build(root)
}

// updateTreePath applies one path change (blob oid, or "" to delete) to a
// tree; returns the new tree oid.
func updateTreePath(treeOid *string, path string, blobOid *string) string {
	segments := strings.Split(path, "/")
	var apply func(current *string, depth int) *string
	apply = func(current *string, depth int) *string {
		var entries []treeEntry
		if current == nil {
			entries = []treeEntry{}
		} else {
			entries = append(entries, parseTree(objects[*current].Data)...)
		}
		name := segments[depth]
		index := -1
		for i, entry := range entries {
			if entry.Name == name {
				index = i
				break
			}
		}
		if depth == len(segments)-1 {
			if blobOid == nil {
				if index >= 0 {
					entries = append(entries[:index], entries[index+1:]...)
				}
			} else if index >= 0 {
				entries[index] = treeEntry{Mode: "100644", Name: name, Oid: *blobOid}
			} else {
				entries = append(entries, treeEntry{Mode: "100644", Name: name, Oid: *blobOid})
			}
		} else {
			var childOid *string
			if index >= 0 && entries[index].Mode == "40000" {
				childOid = &entries[index].Oid
			}
			newChild := apply(childOid, depth+1)
			if newChild == nil || *newChild == emptyTreeOID {
				if index >= 0 {
					entries = append(entries[:index], entries[index+1:]...)
				}
			} else if index >= 0 {
				entries[index] = treeEntry{Mode: "40000", Name: name, Oid: *newChild}
			} else {
				entries = append(entries, treeEntry{Mode: "40000", Name: name, Oid: *newChild})
			}
		}
		if len(entries) == 0 {
			result := emptyTreeOID
			return &result
		}
		result := storeTree(entries)
		return &result
	}
	result := apply(treeOid, 0)
	if result == nil {
		return emptyTreeOID
	}
	return *result
}

// ---------------------------------------------------------------------------
// Repositories, issues, pull requests

type commentRec struct {
	ID   int
	Body string
}

type issueRec struct {
	ID       int
	Number   int
	Title    string
	Body     string
	State    string // "open" | "closed"
	Labels   []string
	Comments []*commentRec
}

type pullRec struct {
	ID             int
	Number         int
	Title          string
	Body           string
	HeadRef        string
	BaseRef        string
	CreatedHeadSha string
	CreatedBaseSha string
	State          string // "open" | "closed"
	Merged         bool
	MergedAt       *string
	MergeCommitSha *string
}

type labelRec struct {
	Name  string
	Color string
}

type repoRec struct {
	ID            int
	Owner         string
	Name          string
	Description   string
	Priv          bool
	DefaultBranch string
	// RefsVersion is bumped on every ref change; it keys the git
	// advertisement/fetch caches.
	RefsVersion int
	refs        map[string]string
	refOrder    []string
	labels      map[string]*labelRec
	labelOrder  []string
	issues      map[int]*issueRec
	issueOrder  []int
	pulls       map[int]*pullRec
	pullOrder   []int
	NextNumber  int
}

func (r *repoRec) ref(name string) (string, bool) {
	oid, ok := r.refs[name]
	return oid, ok
}

func (r *repoRec) setRef(name, oid string) {
	if _, ok := r.refs[name]; !ok {
		r.refOrder = append(r.refOrder, name)
	}
	r.refs[name] = oid
	r.RefsVersion++
}

func (r *repoRec) deleteRef(name string) {
	if _, ok := r.refs[name]; !ok {
		return
	}
	delete(r.refs, name)
	for i, existing := range r.refOrder {
		if existing == name {
			r.refOrder = append(r.refOrder[:i], r.refOrder[i+1:]...)
			break
		}
	}
	r.RefsVersion++
}

var reposByKey = map[string]*repoRec{}
var repoKeyOrder []string

var nextRepoID = 1
var nextIssueID = 1
var nextPullID = 1
var nextCommentID = 1

// Forge-wide id indexes, filled at creation time. Global-id lookups (the
// GraphQL node fields) used to scan every repo and every issue/pull in it;
// these maps make them O(1). A hit is revalidated against the live maps
// before it is trusted, and a miss falls back to the original scan.
var reposByID = map[int]*repoRec{}
var issuesByID = map[int]*issueLocation{}
var pullsByID = map[int]*pullLocation{}

type issueLocation struct {
	Repo  *repoRec
	Issue *issueRec
}

type pullLocation struct {
	Repo *repoRec
	Pull *pullRec
}

func repoKey(owner, name string) string {
	return owner + "/" + name
}

func createRepo(owner, name, description string, priv bool) *repoRec {
	repo := &repoRec{
		ID:            nextRepoID,
		Owner:         owner,
		Name:          name,
		Description:   description,
		Priv:          priv,
		DefaultBranch: "main",
		RefsVersion:   0,
		refs:          map[string]string{},
		labels:        map[string]*labelRec{},
		issues:        map[int]*issueRec{},
		pulls:         map[int]*pullRec{},
		NextNumber:    1,
	}
	nextRepoID++
	key := repoKey(owner, name)
	if _, exists := reposByKey[key]; !exists {
		repoKeyOrder = append(repoKeyOrder, key)
	}
	reposByKey[key] = repo
	reposByID[repo.ID] = repo
	return repo
}

func getRepo(owner, name string) *repoRec {
	return reposByKey[repoKey(owner, name)]
}

func deleteRepo(owner, name string) bool {
	key := repoKey(owner, name)
	if _, ok := reposByKey[key]; !ok {
		return false
	}
	delete(reposByKey, key)
	for i, existing := range repoKeyOrder {
		if existing == key {
			repoKeyOrder = append(repoKeyOrder[:i], repoKeyOrder[i+1:]...)
			break
		}
	}
	return true
}

func viewerRepos() []*repoRec {
	out := []*repoRec{}
	for _, key := range repoKeyOrder {
		repo := reposByKey[key]
		if repo != nil && repo.Owner == viewer.Login {
			out = append(out, repo)
		}
	}
	return out
}

func allRepos() []*repoRec {
	out := []*repoRec{}
	for _, key := range repoKeyOrder {
		if repo := reposByKey[key]; repo != nil {
			out = append(out, repo)
		}
	}
	return out
}

func createIssue(repo *repoRec, title, body string, labels []string) *issueRec {
	issue := &issueRec{
		ID:       nextIssueID,
		Number:   repo.NextNumber,
		Title:    title,
		Body:     body,
		State:    "open",
		Labels:   append([]string{}, labels...),
		Comments: []*commentRec{},
	}
	nextIssueID++
	repo.NextNumber++
	repo.issues[issue.Number] = issue
	repo.issueOrder = append(repo.issueOrder, issue.Number)
	issuesByID[issue.ID] = &issueLocation{Repo: repo, Issue: issue}
	return issue
}

func createPull(repo *repoRec, title, body, headRef, baseRef, headSha, baseSha string) *pullRec {
	pull := &pullRec{
		ID:             nextPullID,
		Number:         repo.NextNumber,
		Title:          title,
		Body:           body,
		HeadRef:        headRef,
		BaseRef:        baseRef,
		CreatedHeadSha: headSha,
		CreatedBaseSha: baseSha,
		State:          "open",
		Merged:         false,
	}
	nextPullID++
	repo.NextNumber++
	repo.pulls[pull.Number] = pull
	repo.pullOrder = append(repo.pullOrder, pull.Number)
	pullsByID[pull.ID] = &pullLocation{Repo: repo, Pull: pull}
	return pull
}

func addComment(issue *issueRec, body string) *commentRec {
	comment := &commentRec{ID: nextCommentID, Body: body}
	nextCommentID++
	issue.Comments = append(issue.Comments, comment)
	return comment
}

// findIssueByID finds an issue anywhere by its forge-wide id (GraphQL
// global-id lookups).
func findIssueByID(id int) *issueLocation {
	if indexed, ok := issuesByID[id]; ok &&
		reposByKey[repoKey(indexed.Repo.Owner, indexed.Repo.Name)] == indexed.Repo &&
		indexed.Repo.issues[indexed.Issue.Number] == indexed.Issue {
		return indexed
	}
	for _, repo := range allRepos() {
		for _, number := range repo.issueOrder {
			issue := repo.issues[number]
			if issue != nil && issue.ID == id {
				return &issueLocation{Repo: repo, Issue: issue}
			}
		}
	}
	return nil
}

func findPullByID(id int) *pullLocation {
	if indexed, ok := pullsByID[id]; ok &&
		reposByKey[repoKey(indexed.Repo.Owner, indexed.Repo.Name)] == indexed.Repo &&
		indexed.Repo.pulls[indexed.Pull.Number] == indexed.Pull {
		return indexed
	}
	for _, repo := range allRepos() {
		for _, number := range repo.pullOrder {
			pull := repo.pulls[number]
			if pull != nil && pull.ID == id {
				return &pullLocation{Repo: repo, Pull: pull}
			}
		}
	}
	return nil
}

func findRepoByID(id int) *repoRec {
	if indexed, ok := reposByID[id]; ok && reposByKey[repoKey(indexed.Owner, indexed.Name)] == indexed {
		return indexed
	}
	for _, repo := range allRepos() {
		if repo.ID == id {
			return repo
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Commits made through the API surfaces

var commitClock int64 = 1_735_689_600

func nextTimestamp() int64 {
	commitClock++
	return commitClock
}

func apiIdentity() string {
	return viewer.Name + " <" + viewer.Email + ">"
}

// makeCommit builds and stores a commit object; the message is stored
// verbatim.
func makeCommit(treeOid string, parents []string, message string) string {
	stamp := nextTimestamp()
	who := fmt.Sprintf("%s %d +0000", apiIdentity(), stamp)
	var sb strings.Builder
	sb.WriteString("tree " + treeOid + "\n")
	for _, parent := range parents {
		sb.WriteString("parent " + parent + "\n")
	}
	sb.WriteString("author " + who + "\n")
	sb.WriteString("committer " + who + "\n")
	sb.WriteString("\n")
	sb.WriteString(message)
	return storeObject("commit", []byte(sb.String()))
}

type pathChange struct {
	Path    string
	Content []byte // nil when Delete is set
	Delete  bool
}

// commitChanges commits a set of path changes on a branch, creating the
// branch (and the repository's first commit) when needed. Returns the new
// commit oid.
func commitChanges(repo *repoRec, branch string, changes []pathChange, message string) string {
	ref := "refs/heads/" + branch
	tip, hasTip := repo.ref(ref)
	treeOid := emptyTreeOID
	if hasTip {
		treeOid = commitViewOf(tip).Tree
	}
	for _, change := range changes {
		var blobOid *string
		if !change.Delete {
			oid := storeObject("blob", change.Content)
			blobOid = &oid
		}
		treeOid = updateTreePath(&treeOid, change.Path, blobOid)
	}
	parents := []string{}
	if hasTip {
		parents = []string{tip}
	}
	commit := makeCommit(treeOid, parents, message)
	repo.setRef(ref, commit)
	return commit
}

// treeOfCommit returns the tree oid at a commit, or the empty tree for an
// unknown oid.
func treeOfCommit(commitOid string) string {
	view := commitViewOf(commitOid)
	if view == nil {
		return emptyTreeOID
	}
	return view.Tree
}

var hex40Pattern = func() func(string) bool {
	return func(s string) bool {
		if len(s) != 40 {
			return false
		}
		for i := 0; i < len(s); i++ {
			c := s[i]
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
		return true
	}
}()

// resolveRef resolves a branch name, full ref, or commit sha to a commit
// oid in this repo; "" when it does not resolve.
func resolveRef(repo *repoRec, ref string) string {
	if oid, ok := repo.ref("refs/heads/" + ref); ok {
		return oid
	}
	if oid, ok := repo.ref(ref); ok {
		return oid
	}
	if hex40Pattern(ref) {
		if peeled, ok := peelToCommit(ref); ok {
			return peeled
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Diffs and merges

type fileChange struct {
	Path      string
	Status    string // "added" | "modified" | "removed"
	Additions int
	Deletions int
}

func lineDiffCounts(oldText, newText string) (int, int) {
	if oldText == newText {
		return 0, 0
	}
	var oldLines, newLines []string
	if oldText != "" {
		oldLines = strings.Split(oldText, "\n")
	}
	if newText != "" {
		newLines = strings.Split(newText, "\n")
	}
	if len(oldLines)*len(newLines) > 4_000_000 {
		return len(newLines), len(oldLines)
	}
	// LCS over lines (files in the suite are small).
	width := len(newLines) + 1
	dp := make([]int, (len(oldLines)+1)*width)
	for i := len(oldLines) - 1; i >= 0; i-- {
		for j := len(newLines) - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				dp[i*width+j] = dp[(i+1)*width+(j+1)] + 1
			} else if dp[(i+1)*width+j] > dp[i*width+(j+1)] {
				dp[i*width+j] = dp[(i+1)*width+j]
			} else {
				dp[i*width+j] = dp[i*width+(j+1)]
			}
		}
	}
	common := dp[0]
	return len(newLines) - common, len(oldLines) - common
}

func blobText(oid string, present bool) string {
	if !present {
		return ""
	}
	object, ok := objects[oid]
	if !ok {
		return ""
	}
	return string(object.Data)
}

// diffTrees returns the file changes between two trees (path-sorted).
func diffTrees(baseTree, headTree string) []fileChange {
	base := flattenTree(baseTree)
	head := flattenTree(headTree)
	pathSet := map[string]bool{}
	for path := range base {
		pathSet[path] = true
	}
	for path := range head {
		pathSet[path] = true
	}
	paths := make([]string, 0, len(pathSet))
	for path := range pathSet {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	changes := []fileChange{}
	for _, path := range paths {
		before, hasBefore := base[path]
		after, hasAfter := head[path]
		if hasBefore == hasAfter && before == after {
			continue
		}
		additions, deletions := lineDiffCounts(blobText(before, hasBefore), blobText(after, hasAfter))
		status := "modified"
		if !hasBefore {
			status = "added"
		} else if !hasAfter {
			status = "removed"
		}
		changes = append(changes, fileChange{Path: path, Status: status, Additions: additions, Deletions: deletions})
	}
	return changes
}

// pullChanges returns the file changes a pull request introduces:
// merge-base tree -> head tree.
func pullChanges(repo *repoRec, pull *pullRec) []fileChange {
	headTip, ok := repo.ref("refs/heads/" + pull.HeadRef)
	if !ok {
		headTip = pull.CreatedHeadSha
	}
	baseTip, ok := repo.ref("refs/heads/" + pull.BaseRef)
	if !ok {
		baseTip = pull.CreatedBaseSha
	}
	base := mergeBase(baseTip, headTip)
	if base == "" {
		base = baseTip
	}
	return diffTrees(treeOfCommit(base), treeOfCommit(headTip))
}

// tryMergeTrees performs a three-way tree merge of head into base; reports
// whether it applies cleanly.
func tryMergeTrees(baseTip, headTip string) (string, bool) {
	base := mergeBase(baseTip, headTip)
	if base == "" {
		return "", false
	}
	ancestor := flattenTree(treeOfCommit(base))
	ours := flattenTree(treeOfCommit(baseTip))
	theirs := flattenTree(treeOfCommit(headTip))
	result := map[string]string{}
	for path, oid := range ours {
		result[path] = oid
	}
	pathSet := map[string]bool{}
	for path := range ancestor {
		pathSet[path] = true
	}
	for path := range theirs {
		pathSet[path] = true
	}
	for path := range pathSet {
		inAncestor, hasAncestor := ancestor[path]
		inTheirs, hasTheirs := theirs[path]
		if hasAncestor == hasTheirs && inAncestor == inTheirs {
			continue // head did not change it
		}
		inOurs, hasOurs := ours[path]
		if (hasOurs != hasAncestor || inOurs != inAncestor) && (hasOurs != hasTheirs || inOurs != inTheirs) {
			return "", false // both changed it, differently
		}
		if !hasTheirs {
			delete(result, path)
		} else {
			result[path] = inTheirs
		}
	}
	return treeFromFlat(result), true
}

type mergeOutcome struct {
	OK     bool
	Commit string
	Reason string // "conflict" | "missing"
}

// squashMerge squash-merges a pull request into its base branch; on
// success the pull is updated.
func squashMerge(repo *repoRec, pull *pullRec) mergeOutcome {
	headTip, hasHead := repo.ref("refs/heads/" + pull.HeadRef)
	baseTip, hasBase := repo.ref("refs/heads/" + pull.BaseRef)
	if !hasHead || !hasBase {
		return mergeOutcome{OK: false, Reason: "missing"}
	}
	mergedTree, ok := tryMergeTrees(baseTip, headTip)
	if !ok {
		return mergeOutcome{OK: false, Reason: "conflict"}
	}
	commit := makeCommit(mergedTree, []string{baseTip}, fmt.Sprintf("%s (#%d)", pull.Title, pull.Number))
	repo.setRef("refs/heads/"+pull.BaseRef, commit)
	pull.State = "closed"
	pull.Merged = true
	now := isoNow()
	pull.MergedAt = &now
	pull.MergeCommitSha = &commit
	return mergeOutcome{OK: true, Commit: commit}
}

// mergeableState computes the GraphQL mergeable state for a pull request,
// the way the merge would go.
func mergeableState(repo *repoRec, pull *pullRec) string {
	if pull.State != "open" {
		return "UNKNOWN"
	}
	headTip, hasHead := repo.ref("refs/heads/" + pull.HeadRef)
	baseTip, hasBase := repo.ref("refs/heads/" + pull.BaseRef)
	if !hasHead || !hasBase {
		return "UNKNOWN"
	}
	if _, ok := tryMergeTrees(baseTip, headTip); ok {
		return "MERGEABLE"
	}
	return "CONFLICTING"
}
