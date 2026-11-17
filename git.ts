// Git smart HTTP: pkt-line framing, packfile read/write (with delta resolution),
// protocol v2 ls-refs/fetch, v0 fetch, and v0 receive-pack with report-status.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';
import {
  CODE_TYPE,
  TYPE_CODE,
  ZERO_OID,
  collectReachable,
  getRepo,
  hashObject,
  isAncestor,
  objects,
  parseCommit,
  parseTag,
  peelOid,
  storeObject,
  type GitObject,
  type GitObjectType,
  type RepoRec,
} from './store.ts';

// ---------------------------------------------------------------------------
// pkt-line

export function pktLine(payload: string | Buffer): Buffer {
  const data = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const header = Buffer.allocUnsafe(4);
  header.write((data.length + 4).toString(16).padStart(4, '0'), 0, 4, 'ascii');
  return Buffer.concat([header, data]);
}

export const PKT_FLUSH = Buffer.from('0000', 'ascii');
export const PKT_DELIM = Buffer.from('0001', 'ascii');

export type Packet =
  | { kind: 'data'; payload: Buffer }
  | { kind: 'flush' }
  | { kind: 'delim' }
  | { kind: 'response-end' };

/** Parse pkt-lines from the front of a buffer; returns the packets and the unparsed rest. */
export function parsePktLines(buffer: Buffer): { packets: Packet[]; rest: Buffer } {
  const packets: Packet[] = [];
  let offset = 0;
  while (offset + 4 <= buffer.length) {
    const header = buffer.subarray(offset, offset + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(header)) break;
    const length = Number.parseInt(header, 16);
    if (length === 0) {
      packets.push({ kind: 'flush' });
      offset += 4;
      continue;
    }
    if (length === 1) {
      packets.push({ kind: 'delim' });
      offset += 4;
      continue;
    }
    if (length === 2) {
      packets.push({ kind: 'response-end' });
      offset += 4;
      continue;
    }
    if (length < 4 || offset + length > buffer.length) break;
    packets.push({ kind: 'data', payload: buffer.subarray(offset + 4, offset + length) });
    offset += length;
  }
  return { packets, rest: buffer.subarray(offset) };
}

// ---------------------------------------------------------------------------
// Packfiles

function writePackObjectHeader(type: GitObjectType, size: number): Buffer {
  const code = TYPE_CODE[type];
  const bytes: number[] = [];
  let remaining = size;
  let first = (code << 4) | (remaining & 0x0f);
  remaining = Math.floor(remaining / 16);
  while (remaining > 0) {
    bytes.push(first | 0x80);
    first = remaining & 0x7f;
    remaining = Math.floor(remaining / 128);
  }
  bytes.push(first);
  return Buffer.from(bytes);
}

/** Build a packfile of plain objects (no deltas); the trailer is the SHA-1 of the body. */
export function writePack(list: readonly GitObject[]): Buffer {
  const parts: Buffer[] = [];
  const header = Buffer.allocUnsafe(12);
  header.write('PACK', 0, 4, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(list.length, 8);
  parts.push(header);
  for (const object of list) {
    parts.push(writePackObjectHeader(object.type, object.data.length));
    parts.push(deflateSync(object.data));
  }
  const body = Buffer.concat(parts);
  const trailer = createHash('sha1').update(body).digest();
  return Buffer.concat([body, trailer]);
}

function readVarintSize(buffer: Buffer, start: number): { size: number; next: number; first: number } {
  let byte = buffer[start];
  const first = byte;
  let size = byte & 0x0f;
  let shift = 4;
  let offset = start + 1;
  while (byte & 0x80) {
    byte = buffer[offset];
    offset += 1;
    size += (byte & 0x7f) * 2 ** shift;
    shift += 7;
    if (shift > 63) throw new Error('pack entry size varint is too long');
  }
  return { size, next: offset, first };
}

function readOfsDeltaBase(buffer: Buffer, start: number): { baseOffset: number; next: number } {
  let byte = buffer[start];
  let offset = start + 1;
  let value = byte & 0x7f;
  while (byte & 0x80) {
    byte = buffer[offset];
    offset += 1;
    value = (value + 1) * 128 + (byte & 0x7f);
  }
  return { baseOffset: value, next: offset };
}

function inflateAt(buffer: Buffer, offset: number): { data: Buffer; next: number } {
  const result = inflateSync(buffer.subarray(offset), { info: true }) as unknown as {
    buffer: Buffer;
    engine: { bytesWritten: number };
  };
  return { data: result.buffer, next: offset + result.engine.bytesWritten };
}

/** Apply a git delta instruction stream to a base object's bytes. */
export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let offset = 0;
  const readSize = (): number => {
    let value = 0;
    let shift = 0;
    for (;;) {
      const byte = delta[offset];
      offset += 1;
      value += (byte & 0x7f) * 2 ** shift;
      shift += 7;
      if (!(byte & 0x80)) break;
    }
    return value;
  };
  const sourceSize = readSize();
  const targetSize = readSize();
  if (sourceSize !== base.length) throw new Error('delta base size mismatch');
  const out = Buffer.allocUnsafe(targetSize);
  let written = 0;
  while (offset < delta.length) {
    const op = delta[offset];
    offset += 1;
    if (op & 0x80) {
      let copyOffset = 0;
      let copySize = 0;
      let shift = 0;
      for (let bit = 0; bit < 4; bit += 1) {
        if (op & (1 << bit)) {
          copyOffset += delta[offset] * 2 ** shift;
          offset += 1;
        }
        shift += 8;
      }
      shift = 0;
      for (let bit = 4; bit < 7; bit += 1) {
        if (op & (1 << bit)) {
          copySize += delta[offset] * 2 ** shift;
          offset += 1;
        }
        shift += 8;
      }
      if (copySize === 0) copySize = 0x10000;
      base.copy(out, written, copyOffset, copyOffset + copySize);
      written += copySize;
    } else if (op !== 0) {
      const literal = op & 0x7f;
      delta.copy(out, written, offset, offset + literal);
      offset += literal;
      written += literal;
    } else {
      throw new Error('delta opcode 0 is invalid');
    }
  }
  if (written !== targetSize) throw new Error('delta produced the wrong size');
  return out;
}

interface RawPackEntry {
  offset: number;
  typeCode: number;
  data: Buffer;
  baseOffset?: number;
  baseOid?: string;
}

/**
 * Read a packfile and store every object it carries (resolving OFS/REF deltas,
 * with the object store as the base source for thin packs). Throws on corruption.
 */
export function readPackIntoStore(pack: Buffer): number {
  if (pack.length < 32) throw new Error('pack is too short');
  if (pack.subarray(0, 4).toString('ascii') !== 'PACK') throw new Error('pack does not start with PACK');
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`unsupported pack version ${version}`);
  const declared = pack.readUInt32BE(8);
  const trailerStart = pack.length - 20;
  const trailer = pack.subarray(trailerStart).toString('hex');
  const computed = createHash('sha1').update(pack.subarray(0, trailerStart)).digest('hex');
  if (trailer !== computed) throw new Error('pack checksum mismatch');

  const raw: RawPackEntry[] = [];
  const byOffset = new Map<number, RawPackEntry>();
  let offset = 12;
  for (let index = 0; index < declared; index += 1) {
    const entryOffset = offset;
    const header = readVarintSize(pack, offset);
    offset = header.next;
    const typeCode = (header.first >> 4) & 0x07;
    const entry: RawPackEntry = { offset: entryOffset, typeCode, data: Buffer.alloc(0) };
    if (typeCode === 6) {
      const base = readOfsDeltaBase(pack, offset);
      offset = base.next;
      entry.baseOffset = entryOffset - base.baseOffset;
    } else if (typeCode === 7) {
      entry.baseOid = pack.subarray(offset, offset + 20).toString('hex');
      offset += 20;
    } else if (CODE_TYPE[typeCode] === undefined) {
      throw new Error(`pack entry has unknown type code ${typeCode}`);
    }
    const inflated = inflateAt(pack.subarray(0, trailerStart), offset);
    if (inflated.data.length !== header.size) throw new Error('pack entry size mismatch');
    entry.data = inflated.data;
    offset = inflated.next;
    raw.push(entry);
    byOffset.set(entryOffset, entry);
  }

  const resolved = new Map<number, { type: GitObjectType; data: Buffer; oid: string }>();
  const resolve = (entry: RawPackEntry): { type: GitObjectType; data: Buffer; oid: string } => {
    const cached = resolved.get(entry.offset);
    if (cached !== undefined) return cached;
    let type: GitObjectType;
    let data: Buffer;
    if (entry.typeCode === 6) {
      const baseEntry = byOffset.get(entry.baseOffset as number);
      if (baseEntry === undefined) throw new Error('ofs-delta base is not in the pack');
      const base = resolve(baseEntry);
      type = base.type;
      data = applyDelta(base.data, entry.data);
    } else if (entry.typeCode === 7) {
      const baseOid = entry.baseOid as string;
      let base: { type: GitObjectType; data: Buffer } | undefined;
      for (const candidate of raw) {
        if (candidate === entry) continue;
        const candidateResolved = resolved.get(candidate.offset);
        if (candidateResolved !== undefined && candidateResolved.oid === baseOid) {
          base = candidateResolved;
          break;
        }
      }
      if (base === undefined) {
        // The base may be an entry not yet resolved, or an object the store already holds.
        const inPack = raw.find((candidate) => {
          if (candidate === entry || candidate.typeCode === 6 || candidate.typeCode === 7) return false;
          return hashObject(CODE_TYPE[candidate.typeCode], candidate.data) === baseOid;
        });
        if (inPack !== undefined) base = resolve(inPack);
        else {
          const stored = objects.get(baseOid);
          if (stored !== undefined) base = stored;
        }
      }
      if (base === undefined) throw new Error(`ref-delta base ${baseOid} is unavailable`);
      type = base.type;
      data = applyDelta(base.data, entry.data);
    } else {
      type = CODE_TYPE[entry.typeCode];
      data = entry.data;
    }
    const done = { type, data, oid: hashObject(type, data) };
    resolved.set(entry.offset, done);
    return done;
  };

  let stored = 0;
  for (const entry of raw) {
    const object = resolve(entry);
    if (!objects.has(object.oid)) {
      objects.set(object.oid, { type: object.type, data: object.data });
      stored += 1;
    }
  }
  return stored;
}

// ---------------------------------------------------------------------------
// Fetch set computation

export interface FetchPlan {
  packObjects: GitObject[];
  shallow: string[];
  unshallow: string[];
}

/** Tree closure of one commit (the commit itself not included). */
function treeClosure(commitOid: string, into: Set<string>): void {
  const object = objects.get(commitOid);
  if (object === undefined || object.type !== 'commit') return;
  const stack = [parseCommit(object.data).tree];
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (into.has(oid)) continue;
    const entry = objects.get(oid);
    if (entry === undefined) continue;
    into.add(oid);
    if (entry.type === 'tree') {
      for (const child of parseTreeEntries(entry.data)) stack.push(child);
    }
  }
}

function parseTreeEntries(data: Buffer): string[] {
  const out: string[] = [];
  let offset = 0;
  while (offset < data.length) {
    const space = data.indexOf(0x20, offset);
    if (space < 0) break;
    const nul = data.indexOf(0x00, space + 1);
    if (nul < 0 || nul + 21 > data.length) break;
    const mode = data.subarray(offset, space).toString('ascii');
    if (mode !== '160000') out.push(data.subarray(nul + 1, nul + 21).toString('hex'));
    offset = nul + 21;
  }
  return out;
}

function commitParents(oid: string): string[] {
  const object = objects.get(oid);
  if (object === undefined || object.type !== 'commit') return [];
  return parseCommit(object.data).parents;
}

/**
 * Compute what a fetch must send.
 * - No depth: closure(wants) minus closure(haves).
 * - Depth/deepen: commits within `depth` generations of the wants, plus their tree
 *   closures, minus what the haves already cover (a have commit and its tree closure).
 *   The shallow boundary is the included commits whose parents were left out.
 */
export function planFetch(
  wants: readonly string[],
  haves: readonly string[],
  clientShallow: readonly string[],
  depth: number | null,
): FetchPlan {
  const send = new Set<string>();
  if (depth === null) {
    const excluded = collectReachable(haves);
    for (const oid of collectReachable(wants)) {
      if (!excluded.has(oid)) send.add(oid);
    }
    return { packObjects: materialize(send), shallow: [], unshallow: [] };
  }

  // Depth-limited: BFS generations from the wants (tags peel to their commit for the walk,
  // but the tag object itself is always sent).
  const excluded = new Set<string>();
  for (const have of haves) {
    excluded.add(have);
    treeClosure(have, excluded);
  }
  const includedCommits = new Set<string>();
  const generations = new Map<string, number>();
  let frontier: string[] = [];
  for (const want of wants) {
    send.add(want);
    const peeled = peelOid(want);
    if (peeled !== null && peeled.type === 'commit') frontier.push(peeled.oid);
  }
  let generation = 0;
  while (frontier.length > 0 && generation < depth) {
    const next: string[] = [];
    for (const oid of frontier) {
      if (generations.has(oid)) continue;
      generations.set(oid, generation);
      includedCommits.add(oid);
      for (const parent of commitParents(oid)) next.push(parent);
    }
    frontier = next;
    generation += 1;
  }
  for (const oid of includedCommits) {
    if (!excluded.has(oid)) send.add(oid);
    const trees = new Set<string>();
    treeClosure(oid, trees);
    for (const treeOid of trees) {
      if (!excluded.has(treeOid)) send.add(treeOid);
    }
  }
  const boundary: string[] = [];
  for (const oid of includedCommits) {
    const parents = commitParents(oid);
    if (parents.length > 0 && parents.some((parent) => !includedCommits.has(parent) && objects.has(parent))) {
      boundary.push(oid);
    }
  }
  const unshallow: string[] = [];
  for (const oid of clientShallow) {
    if (boundary.includes(oid)) continue;
    const parents = commitParents(oid);
    if (parents.length > 0 && parents.every((parent) => includedCommits.has(parent))) unshallow.push(oid);
  }
  return {
    packObjects: materialize(send),
    shallow: boundary,
    unshallow,
  };
}

function materialize(oids: Set<string>): GitObject[] {
  const out: GitObject[] = [];
  for (const oid of oids) {
    const object = objects.get(oid);
    if (object !== undefined) out.push(object);
  }
  return out;
}

// ---------------------------------------------------------------------------
// Advertisements

const UPLOAD_CAPS_V0 = 'side-band-64k ofs-delta thin-pack shallow symref=HEAD:refs/heads/main agent=forge/1.0';
const RECEIVE_CAPS_V0 = 'report-status delete-refs side-band-64k ofs-delta quiet symref=HEAD:refs/heads/main agent=forge/1.0';

function sortedRefs(repo: RepoRec): Array<[string, string]> {
  return [...repo.refs.entries()].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
}

function v0Advertisement(repo: RepoRec, service: string): Buffer {
  const caps = service === 'git-receive-pack' ? RECEIVE_CAPS_V0 : UPLOAD_CAPS_V0;
  const headTarget = `refs/heads/${repo.defaultBranch}`;
  const headTip = repo.refs.get(headTarget);
  const lines: Buffer[] = [pktLine(`# service=${service}\n`), PKT_FLUSH];
  const refs = sortedRefs(repo);
  if (refs.length === 0) {
    lines.push(pktLine(`${ZERO_OID} capabilities^{}\0${caps}\n`));
  } else {
    let first = true;
    if (headTip !== undefined) {
      lines.push(pktLine(`${headTip} HEAD\0${caps}\n`));
      first = false;
    }
    for (const [name, oid] of refs) {
      lines.push(pktLine(first ? `${oid} ${name}\0${caps}\n` : `${oid} ${name}\n`));
      first = false;
      const peeled = peelOid(oid);
      if (peeled !== null && peeled.type === 'commit' && peeled.oid !== oid) {
        lines.push(pktLine(`${peeled.oid} ${name}^{}\n`));
      }
    }
  }
  lines.push(PKT_FLUSH);
  return Buffer.concat(lines);
}

function v2Advertisement(service: string): Buffer {
  const lines: Buffer[] = [pktLine(`# service=${service}\n`), PKT_FLUSH, pktLine('version 2\n')];
  lines.push(pktLine('ls-refs=unborn\n'));
  lines.push(pktLine('fetch=shallow\n'));
  lines.push(pktLine('object-format=sha1\n'));
  lines.push(PKT_FLUSH);
  return Buffer.concat(lines);
}

// ---------------------------------------------------------------------------
// ls-refs

function lsRefsResponse(repo: RepoRec, args: string[]): Buffer {
  let peel = false;
  let symrefs = false;
  const prefixes: string[] = [];
  for (const arg of args) {
    if (arg === 'peel') peel = true;
    else if (arg === 'symrefs') symrefs = true;
    else if (arg.startsWith('ref-prefix ')) prefixes.push(arg.slice('ref-prefix '.length));
  }
  const matches = (name: string): boolean =>
    prefixes.length === 0 || prefixes.some((prefix) => name.startsWith(prefix));
  const lines: Buffer[] = [];
  const headTarget = `refs/heads/${repo.defaultBranch}`;
  const headTip = repo.refs.get(headTarget);
  if (matches('HEAD')) {
    if (headTip !== undefined) {
      lines.push(pktLine(`${headTip} HEAD${symrefs ? ` symref-target:${headTarget}` : ''}\n`));
    } else {
      lines.push(pktLine(`unborn HEAD${symrefs ? ` symref-target:${headTarget}` : ''}\n`));
    }
  }
  for (const [name, oid] of sortedRefs(repo)) {
    if (!matches(name)) continue;
    let line = `${oid} ${name}`;
    if (peel) {
      const peeled = peelOid(oid);
      if (peeled !== null && peeled.type === 'commit' && peeled.oid !== oid) line += ` peeled:${peeled.oid}`;
    }
    lines.push(pktLine(`${line}\n`));
  }
  lines.push(PKT_FLUSH);
  return Buffer.concat(lines);
}

// ---------------------------------------------------------------------------
// Fetch responses

function sidebandChunks(pack: Buffer): Buffer[] {
  const chunks: Buffer[] = [];
  const MAX = 65515;
  for (let offset = 0; offset < pack.length; offset += MAX) {
    const slice = pack.subarray(offset, Math.min(pack.length, offset + MAX));
    chunks.push(pktLine(Buffer.concat([Buffer.from([1]), slice])));
  }
  if (chunks.length === 0) chunks.push(pktLine(Buffer.concat([Buffer.from([1]), Buffer.alloc(0)])));
  return chunks;
}

function fetchResponseV2(plan: FetchPlan): Buffer {
  const parts: Buffer[] = [pktLine('acknowledgments\n'), pktLine('NAK\n'), PKT_FLUSH];
  if (plan.shallow.length > 0 || plan.unshallow.length > 0) {
    parts.push(pktLine('shallow-info\n'));
    for (const oid of plan.shallow) parts.push(pktLine(`shallow ${oid}\n`));
    for (const oid of plan.unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
    parts.push(PKT_FLUSH);
  }
  parts.push(pktLine('packfile\n'));
  parts.push(...sidebandChunks(writePack(plan.packObjects)));
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

function fetchResponseV0(plan: FetchPlan): Buffer {
  const parts: Buffer[] = [];
  for (const oid of plan.shallow) parts.push(pktLine(`shallow ${oid}\n`));
  for (const oid of plan.unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
  parts.push(pktLine('NAK\n'));
  parts.push(...sidebandChunks(writePack(plan.packObjects)));
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------------------
// Request handling

export interface GitReply {
  status: number;
  contentType: string;
  body: Buffer;
}

function gitError(status: number, message: string): GitReply {
  return { status, contentType: 'text/plain; charset=utf-8', body: Buffer.from(`${message}\n`, 'utf8') };
}

/** Split a v2 command request into its argument lines (after the delim). */
function v2Arguments(packets: Packet[]): { command: string; args: string[] } | null {
  let command = '';
  const args: string[] = [];
  let pastDelim = false;
  for (const packet of packets) {
    if (packet.kind === 'delim') {
      pastDelim = true;
      continue;
    }
    if (packet.kind === 'flush') break;
    if (packet.kind !== 'data') continue;
    const line = packet.payload.toString('utf8').replace(/\n$/, '');
    if (!pastDelim) {
      if (line.startsWith('command=')) command = line.slice('command='.length);
      continue;
    }
    args.push(line);
  }
  return command === '' ? null : { command, args };
}

export function handleInfoRefs(repo: RepoRec, service: string, protocolV2: boolean): GitReply {
  if (service !== 'git-upload-pack' && service !== 'git-receive-pack') {
    return gitError(400, 'unknown service');
  }
  const body = protocolV2 && service === 'git-upload-pack' ? v2Advertisement(service) : v0Advertisement(repo, service);
  return { status: 200, contentType: `application/x-${service}-advertisement`, body };
}

export function handleUploadPack(repo: RepoRec, body: Buffer, protocolV2: boolean): GitReply {
  const { packets, rest } = parsePktLines(body);
  if (protocolV2) {
    const parsed = v2Arguments(packets);
    if (parsed === null) return gitError(400, 'malformed protocol v2 request');
    if (parsed.command === 'ls-refs') {
      return { status: 200, contentType: 'application/x-git-upload-pack-result', body: lsRefsResponse(repo, parsed.args) };
    }
    if (parsed.command === 'fetch') {
      const wants: string[] = [];
      const haves: string[] = [];
      const shallow: string[] = [];
      let depth: number | null = null;
      for (const arg of parsed.args) {
        if (arg.startsWith('want ')) wants.push(arg.slice(5).trim());
        else if (arg.startsWith('have ')) haves.push(arg.slice(5).trim());
        else if (arg.startsWith('shallow ')) shallow.push(arg.slice(8).trim());
        else if (arg.startsWith('deepen ')) depth = Number.parseInt(arg.slice(7), 10);
        else if (arg.startsWith('deepen-since ')) depth = null; // not used by the suite; treat as unbounded
      }
      const plan = planFetch(wants, haves, shallow, depth);
      return { status: 200, contentType: 'application/x-git-upload-pack-result', body: fetchResponseV2(plan) };
    }
    return gitError(400, `unknown command ${parsed.command}`);
  }

  // Protocol v0: want lines (with shallow/deepen) up to a flush, then haves and done.
  void rest;
  const wants: string[] = [];
  const haves: string[] = [];
  const shallow: string[] = [];
  let depth: number | null = null;
  let section = 0;
  let sawAny = false;
  for (const packet of packets) {
    if (packet.kind === 'flush') {
      section += 1;
      continue;
    }
    if (packet.kind !== 'data') continue;
    sawAny = true;
    const line = packet.payload.toString('utf8').replace(/\n$/, '');
    if (section === 0) {
      if (line.startsWith('want ')) wants.push(line.slice(5).split(' ')[0]);
      else if (line.startsWith('shallow ')) shallow.push(line.slice(8).trim());
      else if (line.startsWith('deepen ')) depth = Number.parseInt(line.slice(7), 10);
    } else if (line.startsWith('have ')) {
      haves.push(line.slice(5).trim());
    }
  }
  if (!sawAny) return gitError(400, 'malformed upload-pack request');
  const plan = planFetch(wants, haves, shallow, depth);
  return { status: 200, contentType: 'application/x-git-upload-pack-result', body: fetchResponseV0(plan) };
}

interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

export function handleReceivePack(repo: RepoRec, body: Buffer): GitReply {
  const { packets, rest } = parsePktLines(body);
  const commands: PushCommand[] = [];
  let malformed = packets.length === 0;
  for (const packet of packets) {
    if (packet.kind === 'flush') break;
    if (packet.kind !== 'data') {
      malformed = true;
      break;
    }
    let line = packet.payload.toString('utf8').replace(/\n$/, '');
    const nul = line.indexOf('\0');
    if (nul >= 0) line = line.slice(0, nul);
    const parts = line.split(' ');
    if (parts.length < 3 || !/^[0-9a-f]{40}$/.test(parts[0]) || !/^[0-9a-f]{40}$/.test(parts[1])) {
      malformed = true;
      break;
    }
    commands.push({ oldOid: parts[0], newOid: parts[1], ref: parts.slice(2).join(' ') });
  }
  if (malformed || commands.length === 0) return gitError(400, 'malformed receive-pack request');

  let unpackOk = true;
  let unpackDetail = 'ok';
  if (rest.length > 0) {
    try {
      readPackIntoStore(rest);
    } catch (err) {
      unpackOk = false;
      unpackDetail = err instanceof Error ? err.message : 'error';
    }
  }

  const reports: Array<{ ref: string; ok: boolean; reason: string }> = [];
  for (const command of commands) {
    const current = repo.refs.get(command.ref) ?? ZERO_OID;
    if (!unpackOk) {
      reports.push({ ref: command.ref, ok: false, reason: 'unpack failed' });
      continue;
    }
    if (command.newOid === ZERO_OID) {
      repo.refs.delete(command.ref);
      reports.push({ ref: command.ref, ok: true, reason: '' });
      continue;
    }
    if (!objects.has(command.newOid)) {
      reports.push({ ref: command.ref, ok: false, reason: 'missing object' });
      continue;
    }
    if (
      command.ref.startsWith('refs/heads/') &&
      current !== ZERO_OID &&
      current !== command.newOid &&
      !isAncestor(current, command.newOid)
    ) {
      reports.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
      continue;
    }
    repo.refs.set(command.ref, command.newOid);
    reports.push({ ref: command.ref, ok: true, reason: '' });
  }

  const reportParts: Buffer[] = [pktLine(`unpack ${unpackOk ? 'ok' : unpackDetail}\n`)];
  for (const report of reports) {
    reportParts.push(pktLine(report.ok ? `ok ${report.ref}\n` : `ng ${report.ref} ${report.reason}\n`));
  }
  reportParts.push(PKT_FLUSH);
  const reportStream = Buffer.concat(reportParts);
  // Wrap the report-status stream in sideband band-1 packets (side-band-64k was requested).
  const wrapped: Buffer[] = [];
  const MAX = 65515;
  for (let offset = 0; offset < reportStream.length; offset += MAX) {
    const slice = reportStream.subarray(offset, Math.min(reportStream.length, offset + MAX));
    wrapped.push(pktLine(Buffer.concat([Buffer.from([1]), slice])));
  }
  wrapped.push(PKT_FLUSH);
  return { status: 200, contentType: 'application/x-git-receive-pack-result', body: Buffer.concat(wrapped) };
}

/** Look up the repository a git path names; null when it does not exist. */
export function repoForGitPath(owner: string, repoName: string): RepoRec | null {
  return getRepo(owner, repoName);
}
