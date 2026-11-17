// The forge's in-memory store: the git object model, repositories, issues, pull requests,
// labels and comments. Everything lives in Maps for the lifetime of the process (one trial);
// the suite creates every fixture it needs through the public surfaces, so no seeding beyond
// the scenario's user logins (derived from wire.ts's seeded RNG, ported below) is required.

import { createHash } from 'node:crypto';

// ---------------------------------------------------------------------------
// Scenario users (a faithful port of wire.ts's Rng + generateUsers, so the
// login the token names is derived, not guessed).

const U64_MASK = (1n << 64n) - 1n;
const SCENARIO_SEED = 0x6a09e667f3bcc909n;

function fnv1a64(text: string): bigint {
  let hash = 0xcbf29ce484222325n;
  for (let i = 0; i < text.length; i += 1) {
    hash ^= BigInt(text.charCodeAt(i));
    hash = (hash * 0x100000001b3n) & U64_MASK;
  }
  return hash;
}

function mix64(value: bigint): bigint {
  let z = (value + 0x9e3779b97f4a7c15n) & U64_MASK;
  z = ((z ^ (z >> 30n)) * 0xbf58476d1ce4e5b9n) & U64_MASK;
  z = ((z ^ (z >> 27n)) * 0x94d049bb133111ebn) & U64_MASK;
  return (z ^ (z >> 31n)) & U64_MASK;
}

class Rng {
  state: bigint;
  constructor(seed: bigint) {
    const masked = seed & U64_MASK;
    this.state = masked === 0n ? SCENARIO_SEED : masked;
  }
  nextU64(): bigint {
    let x = this.state;
    x = (x ^ (x >> 12n)) & U64_MASK;
    x = (x ^ (x << 25n)) & U64_MASK;
    x = (x ^ (x >> 27n)) & U64_MASK;
    this.state = x;
    return (x * 0x2545f4914f6cdd1dn) & U64_MASK;
  }
  nextU32(): number {
    return Number(this.nextU64() >> 32n);
  }
  nextInt(maxExclusive: number): number {
    if (maxExclusive === 1) return 0;
    const range = 4_294_967_296;
    const limit = range - (range % maxExclusive);
    for (;;) {
      const value = this.nextU32();
      if (value < limit) return value % maxExclusive;
    }
  }
  pick<T>(items: readonly T[]): T {
    return items[this.nextInt(items.length)];
  }
}

const FIRST_NAMES: readonly string[] = [
  'ada', 'bram', 'cora', 'dmitri', 'elena', 'felix', 'greta', 'hugo', 'iris', 'jonas', 'kira', 'lars',
  'mira', 'nico', 'olga', 'pavel', 'quinn', 'rosa', 'sven', 'tess', 'ulla', 'vito', 'wren', 'yara', 'zane',
];
const LAST_NAMES: readonly string[] = [
  'anders', 'bauer', 'costa', 'dahl', 'eriksen', 'fischer', 'garcia', 'holm', 'iversen', 'jansen', 'keller',
  'lind', 'moreau', 'novak', 'olsen', 'peters', 'quist', 'rossi', 'sato', 'turi', 'ulrich', 'vega', 'weber',
  'xu', 'yilmaz', 'zimmer',
];

export interface ScenarioUser {
  id: number;
  login: string;
  name: string;
  email: string;
}

function generateUsers(count: number): ScenarioUser[] {
  const rng = new Rng(mix64(SCENARIO_SEED ^ fnv1a64('users')));
  const used = new Set<string>();
  const users: ScenarioUser[] = [];
  for (let i = 0; i < count; i += 1) {
    const first = rng.pick(FIRST_NAMES);
    const last = rng.pick(LAST_NAMES);
    const base = `${first}${last}`.replace(/[^a-z0-9-]/g, '');
    let login = base;
    let suffix = 2;
    while (used.has(login)) {
      login = `${base}-${suffix}`;
      suffix += 1;
    }
    used.add(login);
    users.push({ id: 1000 + i, login, name: `${first} ${last}`, email: `${login}@example.invalid` });
  }
  return users;
}

const USERS = generateUsers(12);
export const VIEWER: ScenarioUser = USERS[0];
export const STRANGER: ScenarioUser = USERS[2];

// ---------------------------------------------------------------------------
// Git object model

export type GitObjectType = 'commit' | 'tree' | 'blob' | 'tag';

export interface GitObject {
  type: GitObjectType;
  data: Buffer;
}

export const objects = new Map<string, GitObject>();
export const ZERO_OID = '0'.repeat(40);
const TYPE_CODE: Record<GitObjectType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
export const CODE_TYPE: Record<number, GitObjectType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };
export { TYPE_CODE };

export function hashObject(type: GitObjectType, data: Buffer): string {
  const hash = createHash('sha1');
  hash.update(Buffer.from(`${type} ${data.length}\0`, 'utf8'));
  hash.update(data);
  return hash.digest('hex');
}

export function storeObject(type: GitObjectType, data: Buffer): string {
  const oid = hashObject(type, data);
  if (!objects.has(oid)) objects.set(oid, { type, data });
  return oid;
}

export interface CommitView {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
}

export function parseCommit(data: Buffer): CommitView {
  const blank = data.indexOf('\n\n');
  const headerEnd = blank < 0 ? data.length : blank;
  const headerText = data.subarray(0, headerEnd).toString('utf8');
  const message = blank < 0 ? '' : data.subarray(blank + 2).toString('utf8');
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  for (const line of headerText.split('\n')) {
    if (!line) continue;
    const space = line.indexOf(' ');
    const key = space < 0 ? line : line.slice(0, space);
    const value = space < 0 ? '' : line.slice(space + 1);
    if (key === 'tree') tree = value;
    else if (key === 'parent') parents.push(value);
    else if (key === 'author') author = value;
    else if (key === 'committer') committer = value;
  }
  return { tree, parents, author, committer, message };
}

export interface TagView {
  object: string;
  type: string;
  tag: string;
  tagger: string;
  message: string;
}

export function parseTag(data: Buffer): TagView {
  const blank = data.indexOf('\n\n');
  const headerEnd = blank < 0 ? data.length : blank;
  const headers = new Map<string, string>();
  for (const line of data.subarray(0, headerEnd).toString('utf8').split('\n')) {
    if (!line) continue;
    const space = line.indexOf(' ');
    if (space < 0) continue;
    headers.set(line.slice(0, space), line.slice(space + 1));
  }
  return {
    object: headers.get('object') ?? '',
    type: headers.get('type') ?? '',
    tag: headers.get('tag') ?? '',
    tagger: headers.get('tagger') ?? '',
    message: blank < 0 ? '' : data.subarray(blank + 2).toString('utf8'),
  };
}

export interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
}

export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let offset = 0;
  while (offset < data.length) {
    const space = data.indexOf(0x20, offset);
    if (space < 0) break;
    const nul = data.indexOf(0x00, space + 1);
    if (nul < 0 || nul + 21 > data.length) break;
    entries.push({
      mode: data.subarray(offset, space).toString('ascii'),
      name: data.subarray(space + 1, nul).toString('utf8'),
      oid: data.subarray(nul + 1, nul + 21).toString('hex'),
    });
    offset = nul + 21;
  }
  return entries;
}

// ---------------------------------------------------------------------------
// Parsed-object caches
//
// Git objects are immutable and content-addressed, so a parse keyed by oid
// stays valid forever. History walks, tree flattening and fetch planning used
// to re-parse the same commits and trees on every request; these caches make
// each object parse at most once. Callers must treat the returned views as
// read-only. The caches clear wholesale past a generous size cap so a long
// process cannot grow them without bound.

const PARSE_CACHE_LIMIT = 100_000;

const commitViewCache = new Map<string, CommitView>();
const tagViewCache = new Map<string, TagView>();
const treeEntriesCache = new Map<string, TreeEntry[]>();

function cachePut<V>(cache: Map<string, V>, key: string, value: V): V {
  if (cache.size >= PARSE_CACHE_LIMIT) cache.clear();
  cache.set(key, value);
  return value;
}

/** The parsed commit at `oid`, or null when it is missing or not a commit. */
export function commitViewOf(oid: string): CommitView | null {
  const cached = commitViewCache.get(oid);
  if (cached !== undefined) return cached;
  const object = objects.get(oid);
  if (object === undefined || object.type !== 'commit') return null;
  return cachePut(commitViewCache, oid, parseCommit(object.data));
}

/** The parsed tag at `oid`, or null when it is missing or not a tag. */
export function tagViewOf(oid: string): TagView | null {
  const cached = tagViewCache.get(oid);
  if (cached !== undefined) return cached;
  const object = objects.get(oid);
  if (object === undefined || object.type !== 'tag') return null;
  return cachePut(tagViewCache, oid, parseTag(object.data));
}

/** The entries of the tree at `oid`, or null when it is missing or not a tree. */
export function treeEntriesOf(oid: string): TreeEntry[] | null {
  const cached = treeEntriesCache.get(oid);
  if (cached !== undefined) return cached;
  const object = objects.get(oid);
  if (object === undefined || object.type !== 'tree') return null;
  return cachePut(treeEntriesCache, oid, parseTree(object.data));
}

function compareTreeNames(a: TreeEntry, b: TreeEntry): number {
  const left = Buffer.from(a.mode === '40000' ? `${a.name}/` : a.name, 'utf8');
  const right = Buffer.from(b.mode === '40000' ? `${b.name}/` : b.name, 'utf8');
  return Buffer.compare(left, right);
}

export function buildTree(entries: readonly TreeEntry[]): Buffer {
  const sorted = [...entries].sort(compareTreeNames);
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
    parts.push(Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

export function storeTree(entries: readonly TreeEntry[]): string {
  return storeObject('tree', buildTree(entries));
}

export const EMPTY_TREE_OID = storeObject('tree', Buffer.alloc(0));

/** Follow tag objects until a non-tag object; null when the chain breaks. */
export function peelOid(oid: string): { oid: string; type: GitObjectType } | null {
  let current = oid;
  for (let depth = 0; depth < 16; depth += 1) {
    const object = objects.get(current);
    if (object === undefined) return null;
    if (object.type !== 'tag') return { oid: current, type: object.type };
    current = (tagViewOf(current) as TagView).object;
  }
  return null;
}

export function peelToCommit(oid: string): string | null {
  const peeled = peelOid(oid);
  return peeled !== null && peeled.type === 'commit' ? peeled.oid : null;
}

/** Every object reachable from `roots` (commits, their trees and parents; tags followed). */
export function collectReachable(roots: Iterable<string>): Set<string> {
  const seen = new Set<string>();
  const queue: string[] = [];
  for (const root of roots) queue.push(root);
  while (queue.length > 0) {
    const oid = queue.pop() as string;
    if (seen.has(oid)) continue;
    const object = objects.get(oid);
    if (object === undefined) continue;
    seen.add(oid);
    if (object.type === 'commit') {
      const commit = commitViewOf(oid) as CommitView;
      queue.push(commit.tree);
      for (const parent of commit.parents) queue.push(parent);
    } else if (object.type === 'tag') {
      queue.push((tagViewOf(oid) as TagView).object);
    } else if (object.type === 'tree') {
      for (const entry of treeEntriesOf(oid) as TreeEntry[]) {
        if (entry.mode === '160000') continue;
        queue.push(entry.oid);
      }
    }
  }
  return seen;
}

/** True when `ancestor` is reachable from `descendant` through commit parents (inclusive). */
export function isAncestor(ancestor: string, descendant: string): boolean {
  if (ancestor === descendant) return true;
  const seen = new Set<string>();
  const queue = [descendant];
  while (queue.length > 0) {
    const oid = queue.pop() as string;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const object = objects.get(oid);
    if (object === undefined || object.type !== 'commit') continue;
    for (const parent of (commitViewOf(oid) as CommitView).parents) {
      if (parent === ancestor) return true;
      queue.push(parent);
    }
  }
  return false;
}

/** All ancestors of a commit (inclusive), mapped to their BFS distance from it. */
export function ancestorDistances(tip: string): Map<string, number> {
  const distances = new Map<string, number>();
  let frontier = [tip];
  let distance = 0;
  while (frontier.length > 0) {
    const next: string[] = [];
    for (const oid of frontier) {
      if (distances.has(oid)) continue;
      distances.set(oid, distance);
      const object = objects.get(oid);
      if (object !== undefined && object.type === 'commit') {
        for (const parent of (commitViewOf(oid) as CommitView).parents) next.push(parent);
      }
    }
    frontier = next;
    distance += 1;
  }
  return distances;
}

/** The best common ancestor of two commits (smallest summed BFS distance), or null. */
export function mergeBase(a: string, b: string): string | null {
  if (a === b) return a;
  const fromA = ancestorDistances(a);
  const fromB = ancestorDistances(b);
  let best: string | null = null;
  let bestScore = Number.POSITIVE_INFINITY;
  for (const [oid, da] of fromA) {
    const db = fromB.get(oid);
    if (db === undefined) continue;
    const score = da + db;
    if (score < bestScore) {
      bestScore = score;
      best = oid;
    }
  }
  return best;
}

// ---------------------------------------------------------------------------
// Trees as flat path maps

/** Flatten a tree (recursively) into path -> blob oid. */
export function flattenTree(treeOid: string): Map<string, string> {
  const out = new Map<string, string>();
  const walk = (oid: string, prefix: string): void => {
    const entries = treeEntriesOf(oid);
    if (entries === null) return;
    for (const entry of entries) {
      const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
      if (entry.mode === '40000') walk(entry.oid, path);
      else out.set(path, entry.oid);
    }
  };
  walk(treeOid, '');
  return out;
}

/** Build (and store) nested trees from a flat path -> blob oid map; returns the root tree oid. */
export function treeFromFlat(flat: ReadonlyMap<string, string>): string {
  interface Node {
    files: Map<string, string>;
    dirs: Map<string, Node>;
  }
  const make = (): Node => ({ files: new Map(), dirs: new Map() });
  const root = make();
  for (const [path, oid] of flat) {
    const segments = path.split('/');
    let node = root;
    for (let i = 0; i < segments.length - 1; i += 1) {
      const name = segments[i];
      if (!node.dirs.has(name)) node.dirs.set(name, make());
      node = node.dirs.get(name) as Node;
    }
    node.files.set(segments[segments.length - 1], oid);
  }
  const build = (node: Node): string => {
    const entries: TreeEntry[] = [];
    for (const [name, oid] of node.files) entries.push({ mode: '100644', name, oid });
    for (const [name, child] of node.dirs) entries.push({ mode: '40000', name, oid: build(child) });
    return storeTree(entries);
  };
  return build(root);
}

/** Apply one path change (blob oid, or null to delete) to a tree; returns the new tree oid. */
export function updateTreePath(treeOid: string | null, path: string, blobOid: string | null): string {
  const segments = path.split('/');
  const apply = (current: string | null, depth: number): string | null => {
    const entries: TreeEntry[] =
      current === null ? [] : [...parseTree((objects.get(current) as GitObject).data)];
    const name = segments[depth];
    const index = entries.findIndex((entry) => entry.name === name);
    if (depth === segments.length - 1) {
      if (blobOid === null) {
        if (index >= 0) entries.splice(index, 1);
      } else if (index >= 0) {
        entries[index] = { mode: '100644', name, oid: blobOid };
      } else {
        entries.push({ mode: '100644', name, oid: blobOid });
      }
    } else {
      const childOid = index >= 0 && entries[index].mode === '40000' ? entries[index].oid : null;
      const newChild = apply(childOid, depth + 1);
      if (newChild === null || newChild === EMPTY_TREE_OID) {
        if (index >= 0) entries.splice(index, 1);
      } else if (index >= 0) {
        entries[index] = { mode: '40000', name, oid: newChild };
      } else {
        entries.push({ mode: '40000', name, oid: newChild });
      }
    }
    if (entries.length === 0) return EMPTY_TREE_OID;
    return storeTree(entries);
  };
  return apply(treeOid, 0) ?? EMPTY_TREE_OID;
}

// ---------------------------------------------------------------------------
// Repositories, issues, pull requests

export interface CommentRec {
  id: number;
  body: string;
}

export interface IssueRec {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  comments: CommentRec[];
}

export interface PullRec {
  id: number;
  number: number;
  title: string;
  body: string;
  headRef: string;
  baseRef: string;
  createdHeadSha: string;
  createdBaseSha: string;
  state: 'open' | 'closed';
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
}

export interface LabelRec {
  name: string;
  color: string;
}

export interface RepoRec {
  id: number;
  owner: string;
  name: string;
  description: string;
  priv: boolean;
  defaultBranch: string;
  /** Bumped on every ref change; keys the git advertisement/fetch caches. */
  refsVersion: number;
  refs: Map<string, string>;
  labels: Map<string, LabelRec>;
  issues: Map<number, IssueRec>;
  pulls: Map<number, PullRec>;
  nextNumber: number;
}

export const repos = new Map<string, RepoRec>();
let nextRepoId = 1;
let nextIssueId = 1;
let nextPullId = 1;
let nextCommentId = 1;

// Forge-wide id indexes, filled at creation time. Global-id lookups (the
// GraphQL node fields) used to scan every repo and every issue/pull in it;
// these maps make them O(1). A hit is revalidated against the live maps
// before it is trusted, and a miss falls back to the original scan.
const reposById = new Map<number, RepoRec>();
const issuesById = new Map<number, { repo: RepoRec; issue: IssueRec }>();
const pullsById = new Map<number, { repo: RepoRec; pull: PullRec }>();

export function repoKey(owner: string, name: string): string {
  return `${owner}/${name}`;
}

export function createRepo(owner: string, name: string, description: string, priv: boolean): RepoRec {
  const repo: RepoRec = {
    id: nextRepoId,
    owner,
    name,
    description,
    priv,
    defaultBranch: 'main',
    refsVersion: 0,
    refs: new Map(),
    labels: new Map(),
    issues: new Map(),
    pulls: new Map(),
    nextNumber: 1,
  };
  nextRepoId += 1;
  repos.set(repoKey(owner, name), repo);
  reposById.set(repo.id, repo);
  return repo;
}

export function getRepo(owner: string, name: string): RepoRec | null {
  return repos.get(repoKey(owner, name)) ?? null;
}

/** Set a ref and bump the repo's refs version (git response caches key on it). */
export function setRef(repo: RepoRec, ref: string, oid: string): void {
  repo.refs.set(ref, oid);
  repo.refsVersion += 1;
}

/** Delete a ref (when present) and bump the repo's refs version. */
export function deleteRef(repo: RepoRec, ref: string): void {
  if (repo.refs.delete(ref)) repo.refsVersion += 1;
}

export function deleteRepo(owner: string, name: string): boolean {
  return repos.delete(repoKey(owner, name));
}

export function viewerRepos(): RepoRec[] {
  return [...repos.values()].filter((repo) => repo.owner === VIEWER.login);
}

export function createIssue(repo: RepoRec, title: string, body: string, labels: string[]): IssueRec {
  const issue: IssueRec = {
    id: nextIssueId,
    number: repo.nextNumber,
    title,
    body,
    state: 'open',
    labels: [...labels],
    comments: [],
  };
  nextIssueId += 1;
  repo.nextNumber += 1;
  repo.issues.set(issue.number, issue);
  issuesById.set(issue.id, { repo, issue });
  return issue;
}

export function createPull(
  repo: RepoRec,
  title: string,
  body: string,
  headRef: string,
  baseRef: string,
  headSha: string,
  baseSha: string,
): PullRec {
  const pull: PullRec = {
    id: nextPullId,
    number: repo.nextNumber,
    title,
    body,
    headRef,
    baseRef,
    createdHeadSha: headSha,
    createdBaseSha: baseSha,
    state: 'open',
    merged: false,
    mergedAt: null,
    mergeCommitSha: null,
  };
  nextPullId += 1;
  repo.nextNumber += 1;
  repo.pulls.set(pull.number, pull);
  pullsById.set(pull.id, { repo, pull });
  return pull;
}

export function addComment(issue: IssueRec, body: string): CommentRec {
  const comment: CommentRec = { id: nextCommentId, body };
  nextCommentId += 1;
  issue.comments.push(comment);
  return comment;
}

/** Find an issue anywhere by its forge-wide id (GraphQL global-id lookups). */
export function findIssueById(id: number): { repo: RepoRec; issue: IssueRec } | null {
  const indexed = issuesById.get(id);
  if (
    indexed !== undefined &&
    repos.get(repoKey(indexed.repo.owner, indexed.repo.name)) === indexed.repo &&
    indexed.repo.issues.get(indexed.issue.number) === indexed.issue
  ) {
    return indexed;
  }
  for (const repo of repos.values()) {
    for (const issue of repo.issues.values()) {
      if (issue.id === id) return { repo, issue };
    }
  }
  return null;
}

export function findPullById(id: number): { repo: RepoRec; pull: PullRec } | null {
  const indexed = pullsById.get(id);
  if (
    indexed !== undefined &&
    repos.get(repoKey(indexed.repo.owner, indexed.repo.name)) === indexed.repo &&
    indexed.repo.pulls.get(indexed.pull.number) === indexed.pull
  ) {
    return indexed;
  }
  for (const repo of repos.values()) {
    for (const pull of repo.pulls.values()) {
      if (pull.id === id) return { repo, pull };
    }
  }
  return null;
}

export function findRepoById(id: number): RepoRec | null {
  const indexed = reposById.get(id);
  if (indexed !== undefined && repos.get(repoKey(indexed.owner, indexed.name)) === indexed) return indexed;
  for (const repo of repos.values()) {
    if (repo.id === id) return repo;
  }
  return null;
}

// ---------------------------------------------------------------------------
// Commits made through the API surfaces

let commitClock = 1_735_689_600;

function nextTimestamp(): number {
  commitClock += 1;
  return commitClock;
}

export function apiIdentity(): string {
  return `${VIEWER.name} <${VIEWER.email}>`;
}

/** Build and store a commit object; the message is stored verbatim. */
export function makeCommit(treeOid: string, parents: readonly string[], message: string): string {
  const stamp = nextTimestamp();
  const who = `${apiIdentity()} ${stamp} +0000`;
  const lines = [`tree ${treeOid}`];
  for (const parent of parents) lines.push(`parent ${parent}`);
  lines.push(`author ${who}`, `committer ${who}`, '');
  return storeObject('commit', Buffer.from(`${lines.join('\n')}\n${message}`, 'utf8'));
}

/**
 * Commit a set of path changes (Buffer content, or null to delete) on a branch,
 * creating the branch (and the repository's first commit) when needed.
 * Returns the new commit oid.
 */
export function commitChanges(
  repo: RepoRec,
  branch: string,
  changes: ReadonlyMap<string, Buffer | null>,
  message: string,
): string {
  const ref = `refs/heads/${branch}`;
  const tip = repo.refs.get(ref);
  let treeOid = tip === undefined ? EMPTY_TREE_OID : (commitViewOf(tip) as CommitView).tree;
  for (const [path, content] of changes) {
    const blobOid = content === null ? null : storeObject('blob', content);
    treeOid = updateTreePath(treeOid, path, blobOid);
  }
  const commit = makeCommit(treeOid, tip === undefined ? [] : [tip], message);
  setRef(repo, ref, commit);
  return commit;
}

/** The tree oid at a commit, or the empty tree for an unknown oid. */
export function treeOfCommit(commitOid: string): string {
  const view = commitViewOf(commitOid);
  return view === null ? EMPTY_TREE_OID : view.tree;
}

/** Resolve a branch name, full ref, or commit sha to a commit oid in this repo. */
export function resolveRef(repo: RepoRec, ref: string): string | null {
  const byBranch = repo.refs.get(`refs/heads/${ref}`);
  if (byBranch !== undefined) return byBranch;
  const byRef = repo.refs.get(ref);
  if (byRef !== undefined) return byRef;
  if (/^[0-9a-f]{40}$/.test(ref)) {
    const peeled = peelToCommit(ref);
    if (peeled !== null) return peeled;
  }
  return null;
}

// ---------------------------------------------------------------------------
// Diffs and merges

export interface FileChange {
  path: string;
  status: 'added' | 'modified' | 'removed';
  additions: number;
  deletions: number;
}

function lineDiffCounts(oldText: string, newText: string): { additions: number; deletions: number } {
  if (oldText === newText) return { additions: 0, deletions: 0 };
  const oldLines = oldText === '' ? [] : oldText.split('\n');
  const newLines = newText === '' ? [] : newText.split('\n');
  if (oldLines.length * newLines.length > 4_000_000) {
    return { additions: newLines.length, deletions: oldLines.length };
  }
  // LCS over lines (files in the suite are small).
  const dp: number[] = new Array((oldLines.length + 1) * (newLines.length + 1)).fill(0);
  const width = newLines.length + 1;
  for (let i = oldLines.length - 1; i >= 0; i -= 1) {
    for (let j = newLines.length - 1; j >= 0; j -= 1) {
      dp[i * width + j] =
        oldLines[i] === newLines[j]
          ? dp[(i + 1) * width + (j + 1)] + 1
          : Math.max(dp[(i + 1) * width + j], dp[i * width + (j + 1)]);
    }
  }
  const common = dp[0];
  return { additions: newLines.length - common, deletions: oldLines.length - common };
}

function blobText(oid: string | undefined): string {
  if (oid === undefined) return '';
  const object = objects.get(oid);
  return object === undefined ? '' : object.data.toString('utf8');
}

/** The file changes between two trees (path-sorted). */
export function diffTrees(baseTree: string, headTree: string): FileChange[] {
  const base = flattenTree(baseTree);
  const head = flattenTree(headTree);
  const paths = new Set<string>([...base.keys(), ...head.keys()]);
  const changes: FileChange[] = [];
  for (const path of [...paths].sort()) {
    const before = base.get(path);
    const after = head.get(path);
    if (before === after) continue;
    const counts = lineDiffCounts(blobText(before), blobText(after));
    changes.push({
      path,
      status: before === undefined ? 'added' : after === undefined ? 'removed' : 'modified',
      additions: counts.additions,
      deletions: counts.deletions,
    });
  }
  return changes;
}

/** The file changes a pull request introduces: merge-base tree -> head tree. */
export function pullChanges(repo: RepoRec, pull: PullRec): FileChange[] {
  const headTip = repo.refs.get(`refs/heads/${pull.headRef}`) ?? pull.createdHeadSha;
  const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`) ?? pull.createdBaseSha;
  const base = mergeBase(baseTip, headTip) ?? baseTip;
  return diffTrees(treeOfCommit(base), treeOfCommit(headTip));
}

export type MergeOutcome =
  | { ok: true; commit: string }
  | { ok: false; reason: 'conflict' | 'missing' };

/** A three-way tree merge of head into base; reports whether it applies cleanly. */
export function tryMergeTrees(
  baseTip: string,
  headTip: string,
): { ok: true; treeOid: string } | { ok: false } {
  const base = mergeBase(baseTip, headTip);
  if (base === null) return { ok: false };
  const ancestor = flattenTree(treeOfCommit(base));
  const ours = flattenTree(treeOfCommit(baseTip));
  const theirs = flattenTree(treeOfCommit(headTip));
  const result = new Map(ours);
  const paths = new Set<string>([...ancestor.keys(), ...theirs.keys()]);
  for (const path of paths) {
    const inAncestor = ancestor.get(path);
    const inTheirs = theirs.get(path);
    if (inAncestor === inTheirs) continue; // head did not change it
    const inOurs = ours.get(path);
    if (inOurs !== inAncestor && inOurs !== inTheirs) return { ok: false }; // both changed it, differently
    if (inTheirs === undefined) result.delete(path);
    else result.set(path, inTheirs);
  }
  return { ok: true, treeOid: treeFromFlat(result) };
}

/** Squash-merge a pull request into its base branch; on success the pull is updated. */
export function squashMerge(repo: RepoRec, pull: PullRec): MergeOutcome {
  const headTip = repo.refs.get(`refs/heads/${pull.headRef}`);
  const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`);
  if (headTip === undefined || baseTip === undefined) return { ok: false, reason: 'missing' };
  const merged = tryMergeTrees(baseTip, headTip);
  if (!merged.ok) return { ok: false, reason: 'conflict' };
  const commit = makeCommit(merged.treeOid, [baseTip], `${pull.title} (#${pull.number})`);
  setRef(repo, `refs/heads/${pull.baseRef}`, commit);
  pull.state = 'closed';
  pull.merged = true;
  pull.mergedAt = new Date().toISOString();
  pull.mergeCommitSha = commit;
  return { ok: true, commit };
}

/** GraphQL mergeable state for a pull request, computed the way the merge would go. */
export function mergeableState(repo: RepoRec, pull: PullRec): 'MERGEABLE' | 'CONFLICTING' | 'UNKNOWN' {
  if (pull.state !== 'open') return 'UNKNOWN';
  const headTip = repo.refs.get(`refs/heads/${pull.headRef}`);
  const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`);
  if (headTip === undefined || baseTip === undefined) return 'UNKNOWN';
  return tryMergeTrees(baseTip, headTip).ok ? 'MERGEABLE' : 'CONFLICTING';
}
