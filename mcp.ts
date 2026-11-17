// The MCP endpoint at /mcp: JSON-RPC over streamable HTTP. Both protocol eras are
// served — the modern server/discover handshake and the legacy initialize handshake —
// with six tools backed by the same store as REST and GraphQL.

import {
  VIEWER,
  createIssue,
  createPull,
  createRepo,
  getRepo,
  squashMerge,
  type IssueRec,
  type PullRec,
  type RepoRec,
} from './store.ts';

const SERVER_INFO = { name: 'forge', version: '1.0.0' };
const SUPPORTED_VERSIONS = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];

interface ToolDef {
  name: string;
  description: string;
  inputSchema: {
    type: 'object';
    properties: Record<string, { type: string; description: string }>;
    required: string[];
  };
}

const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a repository owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'The repository name.' },
        description: { type: 'string', description: 'A short description of the repository.' },
        private: { type: 'boolean', description: 'Whether the repository is private.' },
        owner: { type: 'string', description: 'The repository owner login (defaults to the token owner).' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Read one repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Open an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The issue title.' },
        body: { type: 'string', description: 'The issue body.' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List the issues of a repository, optionally filtered by state.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        state: { type: 'string', description: 'Filter by state: open, closed or all.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Open a pull request from a head branch into a base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The pull request title.' },
        body: { type: 'string', description: 'The pull request body.' },
        head: { type: 'string', description: 'The head branch name.' },
        base: { type: 'string', description: 'The base branch name.' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Squash-merge a pull request into its base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        pull_number: { type: 'number', description: 'The pull request number.' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

const TOOLS_BY_NAME = new Map(TOOLS.map((tool) => [tool.name, tool]));

function repoView(repo: RepoRec): Record<string, unknown> {
  return {
    id: repo.id,
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner },
    private: repo.priv,
    description: repo.description,
    default_branch: repo.defaultBranch,
  };
}

function issueView(repo: RepoRec, issue: IssueRec): Record<string, unknown> {
  return {
    id: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => ({ name })),
    repository: `${repo.owner}/${repo.name}`,
  };
}

function pullView(repo: RepoRec, pull: PullRec): Record<string, unknown> {
  return {
    id: pull.id,
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    merged: pull.merged,
    merge_commit_sha: pull.mergeCommitSha,
    head: { ref: pull.headRef, sha: repo.refs.get(`refs/heads/${pull.headRef}`) ?? pull.createdHeadSha },
    base: { ref: pull.baseRef, sha: repo.refs.get(`refs/heads/${pull.baseRef}`) ?? pull.createdBaseSha },
  };
}

class ToolFailure extends Error {}

function requireRepo(owner: unknown, name: unknown): RepoRec {
  const repo = getRepo(String(owner ?? ''), String(name ?? ''));
  if (repo === null) throw new ToolFailure(`repository ${String(owner)}/${String(name)} not found`);
  return repo;
}

function callTool(name: string, args: Record<string, unknown>): Record<string, unknown> {
  switch (name) {
    case 'create_repo': {
      const repoName = String(args.name);
      if (getRepo(VIEWER.login, repoName) !== null) throw new ToolFailure(`repository ${repoName} already exists`);
      const repo = createRepo(
        VIEWER.login,
        repoName,
        typeof args.description === 'string' ? args.description : '',
        args.private === true,
      );
      return repoView(repo);
    }
    case 'get_repo': {
      return repoView(requireRepo(args.owner, args.repo));
    }
    case 'create_issue': {
      const repo = requireRepo(args.owner, args.repo);
      const issue = createIssue(repo, String(args.title), typeof args.body === 'string' ? args.body : '', []);
      return issueView(repo, issue);
    }
    case 'list_issues': {
      const repo = requireRepo(args.owner, args.repo);
      const state = typeof args.state === 'string' ? args.state : 'open';
      let list = [...repo.issues.values()];
      if (state === 'open' || state === 'closed') list = list.filter((issue) => issue.state === state);
      list.sort((a, b) => a.number - b.number);
      return { issues: list.map((issue) => issueView(repo, issue)) };
    }
    case 'create_pull_request': {
      const repo = requireRepo(args.owner, args.repo);
      const head = String(args.head);
      const base = String(args.base);
      const headSha = repo.refs.get(`refs/heads/${head}`);
      const baseSha = repo.refs.get(`refs/heads/${base}`);
      if (headSha === undefined) throw new ToolFailure(`head branch ${head} does not exist`);
      if (baseSha === undefined) throw new ToolFailure(`base branch ${base} does not exist`);
      const pull = createPull(
        repo,
        String(args.title),
        typeof args.body === 'string' ? args.body : '',
        head,
        base,
        headSha,
        baseSha,
      );
      return pullView(repo, pull);
    }
    case 'merge_pull_request': {
      const repo = requireRepo(args.owner, args.repo);
      const pull = repo.pulls.get(Number(args.pull_number));
      if (pull === undefined) throw new ToolFailure(`pull request ${String(args.pull_number)} not found`);
      if (pull.state === 'open') {
        const outcome = squashMerge(repo, pull);
        if (!outcome.ok) throw new ToolFailure('pull request is not mergeable');
      }
      return pullView(repo, pull);
    }
    default:
      throw new ToolFailure(`unknown tool ${name}`);
  }
}

// ---------------------------------------------------------------------------

export interface McpReply {
  status: number;
  body?: unknown;
}

type JsonRpcId = string | number | null;

function rpcResult(id: JsonRpcId, result: unknown): McpReply {
  return { status: 200, body: { jsonrpc: '2.0', id, result } };
}

function rpcError(id: JsonRpcId, code: number, message: string, status = 200): McpReply {
  return { status, body: { jsonrpc: '2.0', id, error: { code, message } } };
}

function handleOne(message: Record<string, unknown>): McpReply | null {
  const id = (message.id ?? null) as JsonRpcId;
  const method = typeof message.method === 'string' ? message.method : '';
  const params = (message.params ?? {}) as Record<string, unknown>;
  const isNotification = message.id === undefined;

  switch (method) {
    case 'server/discover':
      return rpcResult(id, {
        supportedVersions: SUPPORTED_VERSIONS,
        capabilities: { tools: {} },
        serverInfo: SERVER_INFO,
        _meta: { 'io.modelcontextprotocol/serverInfo': SERVER_INFO },
      });
    case 'initialize':
      return rpcResult(id, {
        protocolVersion: typeof params.protocolVersion === 'string' ? params.protocolVersion : '2025-11-25',
        capabilities: { tools: {} },
        serverInfo: SERVER_INFO,
      });
    case 'ping':
      return rpcResult(id, {});
    case 'tools/list':
      return rpcResult(id, { tools: TOOLS });
    case 'tools/call': {
      const toolName = typeof params.name === 'string' ? params.name : '';
      const tool = TOOLS_BY_NAME.get(toolName);
      if (tool === undefined) return rpcError(id, -32602, `unknown tool: ${toolName}`);
      const args = (params.arguments ?? {}) as Record<string, unknown>;
      const missing = tool.inputSchema.required.filter((key) => args[key] === undefined || args[key] === null || args[key] === '');
      if (missing.length > 0) return rpcError(id, -32602, `missing required arguments: ${missing.join(', ')}`);
      try {
        const result = callTool(toolName, args);
        const text = JSON.stringify(result);
        return rpcResult(id, {
          content: [{ type: 'text', text }],
          structuredContent: result,
          isError: false,
        });
      } catch (err) {
        if (err instanceof ToolFailure) {
          return rpcResult(id, {
            content: [{ type: 'text', text: err.message }],
            isError: true,
          });
        }
        throw err;
      }
    }
    default: {
      if (isNotification || method.startsWith('notifications/')) return null; // answered 202 with no body
      return rpcError(id, -32601, `method not found: ${method}`);
    }
  }
}

/**
 * Handle one POST /mcp. `rawBody` is the exact request text: a body that is not JSON
 * is a JSON-RPC parse error answered HTTP 400 with code -32700.
 */
export function handleMcp(rawBody: string): McpReply {
  let parsed: unknown;
  try {
    parsed = JSON.parse(rawBody);
  } catch {
    return rpcError(null, -32700, 'Parse error', 400);
  }
  if (Array.isArray(parsed)) {
    const replies: unknown[] = [];
    for (const entry of parsed) {
      if (entry !== null && typeof entry === 'object') {
        const reply = handleOne(entry as Record<string, unknown>);
        if (reply !== null && reply.body !== undefined) replies.push(reply.body);
      }
    }
    return replies.length > 0 ? { status: 200, body: replies } : { status: 202 };
  }
  if (parsed === null || typeof parsed !== 'object') {
    return rpcError(null, -32600, 'Invalid Request', 400);
  }
  const reply = handleOne(parsed as Record<string, unknown>);
  if (reply === null) return { status: 202 };
  return reply;
}
