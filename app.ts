// The forge's HTTP server: health probe, git smart HTTP, REST v3, GraphQL v4 and MCP,
// behind the one bearer-token auth the suite speaks. main.ts owns the lifecycle
// (bind, register, poll /done); this file is the whole surface.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { VIEWER } from './store.ts';
import { handleGraphQL } from './graphql.ts';
import { handleInfoRefs, handleReceivePack, handleUploadPack, repoForGitPath } from './git.ts';
import { handleMcp } from './mcp.ts';
import { handleRest } from './rest.ts';

const JSON_TYPE = 'application/json; charset=utf-8';
const TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';

let requestCounter = 0;

function nextRequestId(): string {
  requestCounter += 1;
  return `forge-${requestCounter.toString(36)}-${Date.now().toString(36)}`;
}

function authorized(req: IncomingMessage): boolean {
  const header = req.headers.authorization;
  if (TOKEN !== '') return header === `Bearer ${TOKEN}`;
  return typeof header === 'string' && header.startsWith('Bearer ') && header.length > 'Bearer '.length;
}

function sendJson(res: ServerResponse, status: number, body: unknown, requestId: string, headers: Record<string, string> = {}): void {
  if (status === 204 || body === undefined) {
    res.writeHead(status, { 'x-github-request-id': requestId, ...headers });
    res.end();
    return;
  }
  const payload = Buffer.from(JSON.stringify(body), 'utf8');
  res.writeHead(status, {
    'content-type': JSON_TYPE,
    'content-length': payload.length,
    'x-github-request-id': requestId,
    ...headers,
  });
  res.end(payload);
}

function sendRaw(res: ServerResponse, status: number, contentType: string, body: Buffer, requestId: string): void {
  res.writeHead(status, {
    'content-type': contentType,
    'content-length': body.length,
    'x-github-request-id': requestId,
  });
  res.end(body);
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  // Event-driven collection: the async-iterator form costs noticeably more
  // per request, and this runs on every POST the benchmark times.
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let total = 0;
    req.on('data', (chunk: Buffer) => {
      total += chunk.length;
      if (total > 128 * 1024 * 1024) {
        reject(new Error('request body too large'));
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

function decodeSegment(segment: string): string {
  try {
    return decodeURIComponent(segment);
  } catch {
    return segment;
  }
}

const GIT_PATH = /^\/([^/]+)\/([^/]+)\.git\/(info\/refs|git-upload-pack|git-receive-pack)$/;

async function handle(req: IncomingMessage, res: ServerResponse): Promise<void> {
  const requestId = nextRequestId();
  const method = req.method ?? 'GET';
  let url: URL;
  try {
    url = new URL(req.url ?? '/', 'http://entry');
  } catch {
    sendJson(res, 400, { message: 'Bad Request' }, requestId);
    return;
  }
  const path = url.pathname;

  // The env's boot check: no auth, always up.
  if (method === 'GET' && path === '/healthz') {
    sendJson(res, 200, { ok: true }, requestId);
    return;
  }

  const needsBody = method === 'POST' || method === 'PUT' || method === 'PATCH' || method === 'DELETE';
  const rawBody = needsBody ? await readBody(req) : Buffer.alloc(0);

  // Everything else speaks the one bearer token.
  if (!authorized(req)) {
    sendJson(res, 401, { message: 'Bad credentials' }, requestId);
    return;
  }

  // GraphQL v4.
  if (path === '/api/graphql') {
    if (method !== 'POST') {
      sendJson(res, 404, { message: 'Not Found' }, requestId);
      return;
    }
    let parsed: unknown;
    try {
      parsed = JSON.parse(rawBody.toString('utf8'));
    } catch {
      sendJson(res, 400, { message: 'Problems parsing JSON' }, requestId);
      return;
    }
    const reply = handleGraphQL(parsed);
    sendJson(res, reply.status, reply.body, requestId);
    return;
  }

  // MCP.
  if (path === '/mcp') {
    if (method === 'DELETE') {
      sendJson(res, 404, { message: 'Not Found' }, requestId);
      return;
    }
    if (method !== 'POST') {
      sendJson(res, 404, { message: 'Not Found' }, requestId);
      return;
    }
    const reply = handleMcp(rawBody.toString('utf8'));
    if (reply.body === undefined) {
      res.writeHead(reply.status, { 'x-github-request-id': requestId });
      res.end();
      return;
    }
    sendJson(res, reply.status, reply.body, requestId);
    return;
  }

  // REST v3.
  if (path === '/api/v3' || path.startsWith('/api/v3/')) {
    const tail = path.slice('/api/v3'.length).replace(/\/+$/, '');
    const segments = tail === '' ? [] : tail.split('/').slice(1).map(decodeSegment);
    let body: Record<string, unknown> | null = null;
    let bodyError = false;
    if (rawBody.length > 0) {
      try {
        const parsed = JSON.parse(rawBody.toString('utf8')) as unknown;
        body = parsed !== null && typeof parsed === 'object' && !Array.isArray(parsed) ? (parsed as Record<string, unknown>) : {};
      } catch {
        bodyError = true;
      }
    }
    const reply = handleRest({
      method,
      segments,
      query: url.searchParams,
      body,
      bodyError,
      host: req.headers.host ?? 'localhost',
    });
    sendJson(res, reply.status, reply.body, requestId, reply.headers ?? {});
    return;
  }

  // Git smart HTTP.
  const gitMatch = GIT_PATH.exec(path);
  if (gitMatch !== null) {
    const owner = decodeSegment(gitMatch[1]);
    const repoName = decodeSegment(gitMatch[2]);
    const repo = repoForGitPath(owner, repoName);
    if (repo === null) {
      sendRaw(res, 404, 'text/plain; charset=utf-8', Buffer.from('repository not found\n'), requestId);
      return;
    }
    const endpoint = gitMatch[3];
    const protocolV2 = (req.headers['git-protocol'] ?? '').includes('version=2');
    if (endpoint === 'info/refs' && method === 'GET') {
      const service = url.searchParams.get('service') ?? '';
      const reply = handleInfoRefs(repo, service, protocolV2);
      sendRaw(res, reply.status, reply.contentType, reply.body, requestId);
      return;
    }
    if (endpoint === 'git-upload-pack' && method === 'POST') {
      const reply = handleUploadPack(repo, rawBody, protocolV2);
      sendRaw(res, reply.status, reply.contentType, reply.body, requestId);
      return;
    }
    if (endpoint === 'git-receive-pack' && method === 'POST') {
      const reply = handleReceivePack(repo, rawBody);
      sendRaw(res, reply.status, reply.contentType, reply.body, requestId);
      return;
    }
    sendJson(res, 404, { message: 'Not Found' }, requestId);
    return;
  }

  void VIEWER;
  sendJson(res, 404, { message: 'Not Found' }, requestId);
}

/** Build the server. It binds nothing until the caller listens; main.ts listens on an ephemeral port. */
export function createApp(): Server {
  const server = createServer((req, res) => {
    handle(req, res).catch(() => {
      try {
        if (!res.headersSent) sendJson(res, 500, { message: 'Internal Server Error' }, nextRequestId());
        else res.end();
      } catch {
        // The socket is already gone; nothing more to do.
      }
    });
  });
  server.keepAliveTimeout = 30_000;
  return server;
}
