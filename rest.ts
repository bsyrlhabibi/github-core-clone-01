// The GitHub REST v3 surface: repositories, branches, commits, contents, labels,
// issues, comments and pull requests, with GitHub's statuses and JSON shapes.

import {
  VIEWER,
  commitChanges,
  createIssue,
  createPull,
  createRepo,
  addComment,
  deleteRepo,
  flattenTree,
  getRepo,
  objects,
  parseCommit,
  parseTree,
  pullChanges,
  resolveRef,
  squashMerge,
  storeObject,
  treeOfCommit,
  viewerRepos,
  type IssueRec,
  type PullRec,
  type RepoRec,
} from './store.ts';

export interface RestRequest {
  method: string;
  /** Decoded path segments after /api/v3. */
  segments: string[];
  query: URLSearchParams;
  body: Record<string, unknown> | null;
  /** True when a request body was present but not valid JSON. */
  bodyError: boolean;
  /** The Host header, for absolute pagination URLs. */
  host: string;
}

export interface RestReply {
  status: number;
  body?: unknown;
  headers?: Record<string, string>;
}

const NOT_FOUND: RestReply = { status: 404, body: { message: 'Not Found' } };

function message(status: number, text: string): RestReply {
  return { status, body: { message: text } };
}

function repoJson(repo: RepoRec): Record<string, unknown> {
  return {
    id: repo.id,
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner, id: VIEWER.id, type: 'User' },
    private: repo.priv,
    description: repo.description,
    default_branch: repo.defaultBranch,
  };
}

function labelJson(name: string, color: string): Record<string, unknown> {
  return { name, color, description: null, default: false };
}

function issueJson(repo: RepoRec, issue: IssueRec): Record<string, unknown> {
  return {
    id: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => labelJson(name, repo.labels.get(name)?.color ?? 'ededed')),
    user: { login: repo.owner, id: VIEWER.id, type: 'User' },
    comments: issue.comments.length,
  };
}

function commentJson(body: string, id: number, owner: string): Record<string, unknown> {
  return { id, body, user: { login: owner, id: VIEWER.id, type: 'User' } };
}

function pullJson(repo: RepoRec, pull: PullRec): Record<string, unknown> {
  const headSha = repo.refs.get(`refs/heads/${pull.headRef}`) ?? pull.createdHeadSha;
  const baseSha = repo.refs.get(`refs/heads/${pull.baseRef}`) ?? pull.createdBaseSha;
  return {
    id: pull.id,
    number: pull.number,
    state: pull.state,
    title: pull.title,
    body: pull.body,
    head: { ref: pull.headRef, sha: headSha, label: `${repo.owner}:${pull.headRef}` },
    base: { ref: pull.baseRef, sha: baseSha, label: `${repo.owner}:${pull.baseRef}` },
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    user: { login: repo.owner, id: VIEWER.id, type: 'User' },
  };
}

function parsePage(query: URLSearchParams): { page: number; perPage: number } {
  const perPageRaw = Number.parseInt(query.get('per_page') ?? '', 10);
  const pageRaw = Number.parseInt(query.get('page') ?? '', 10);
  const perPage = Number.isFinite(perPageRaw) && perPageRaw > 0 ? Math.min(perPageRaw, 100) : 30;
  const page = Number.isFinite(pageRaw) && pageRaw > 0 ? pageRaw : 1;
  return { page, perPage };
}

function paginate<T>(items: T[], query: URLSearchParams, basePath: string, host: string): { page: T[]; headers: Record<string, string> } {
  const { page, perPage } = parsePage(query);
  const start = (page - 1) * perPage;
  const slice = items.slice(start, start + perPage);
  const headers: Record<string, string> = {};
  const lastPage = Math.max(1, Math.ceil(items.length / perPage));
  if (page < lastPage) {
    const url = (p: number): string => `http://${host}${basePath}?per_page=${perPage}&page=${p}`;
    const parts = [`<${url(page + 1)}>; rel="next"`, `<${url(lastPage)}>; rel="last"`];
    if (page > 1) parts.push(`<${url(1)}>; rel="first"`, `<${url(page - 1)}>; rel="prev"`);
    headers.link = parts.join(', ');
  }
  return { page: slice, headers };
}

// ---------------------------------------------------------------------------

export function handleRest(req: RestRequest): RestReply {
  const seg = req.segments;
  const method = req.method;

  if (req.bodyError && (method === 'POST' || method === 'PUT' || method === 'PATCH' || method === 'DELETE')) {
    return message(400, 'Problems parsing JSON');
  }

  // API root and meta.
  if (seg.length === 0) {
    if (method !== 'GET') return NOT_FOUND;
    return {
      status: 200,
      body: {
        current_user_url: '/api/v3/user',
        repository_url: '/api/v3/repos/{owner}/{repo}',
        issue_search_url: '/api/v3/search/issues',
        rate_limit_url: '/api/v3/rate_limit',
      },
      headers: { 'x-oauth-scopes': 'repo, read:org' },
    };
  }
  if (seg.length === 1 && seg[0] === 'meta') {
    if (method !== 'GET') return NOT_FOUND;
    return { status: 200, body: { installed_version: '1.0.0', verifiable_password_authentication: false } };
  }
  if (seg.length === 1 && seg[0] === 'user') {
    if (method !== 'GET') return NOT_FOUND;
    return { status: 200, body: { login: VIEWER.login, id: VIEWER.id, name: VIEWER.name, email: VIEWER.email, type: 'User' } };
  }

  // The viewer's repositories.
  if (seg.length === 2 && seg[0] === 'user' && seg[1] === 'repos') {
    if (method === 'POST') {
      const name = req.body?.name;
      if (typeof name !== 'string' || name === '') return message(422, 'Validation Failed: name is required');
      if (getRepo(VIEWER.login, name) !== null) return message(422, 'Repository already exists');
      const description = typeof req.body?.description === 'string' ? req.body.description : '';
      const repo = createRepo(VIEWER.login, name, description, req.body?.private === true);
      return { status: 201, body: repoJson(repo) };
    }
    if (method === 'GET') {
      const all = viewerRepos().map(repoJson);
      const { page, headers } = paginate(all, req.query, '/api/v3/user/repos', req.host);
      return { status: 200, body: page, headers };
    }
    return NOT_FOUND;
  }

  if (seg.length >= 3 && seg[0] === 'repos') {
    const owner = seg[1];
    const name = seg[2];
    const repo = getRepo(owner, name);
    if (repo === null) return NOT_FOUND;
    return handleRepo(req, repo, seg.slice(3));
  }

  return NOT_FOUND;
}

function handleRepo(req: RestRequest, repo: RepoRec, seg: string[]): RestReply {
  const method = req.method;

  if (seg.length === 0) {
    if (method === 'GET') return { status: 200, body: repoJson(repo) };
    if (method === 'PATCH') {
      if (typeof req.body?.description === 'string') repo.description = req.body.description;
      if (typeof req.body?.private === 'boolean') repo.priv = req.body.private;
      if (typeof req.body?.default_branch === 'string' && req.body.default_branch !== '') {
        repo.defaultBranch = req.body.default_branch;
      }
      return { status: 200, body: repoJson(repo) };
    }
    if (method === 'DELETE') {
      deleteRepo(repo.owner, repo.name);
      return { status: 204 };
    }
    return NOT_FOUND;
  }

  if (seg[0] === 'branches') {
    if (seg.length === 1 && method === 'GET') {
      const branches = [...repo.refs.entries()]
        .filter(([ref]) => ref.startsWith('refs/heads/'))
        .map(([ref, sha]) => ({ name: ref.slice('refs/heads/'.length), commit: { sha }, protected: false }));
      return { status: 200, body: branches };
    }
    if (seg.length >= 2 && method === 'GET') {
      const branch = seg.slice(1).join('/');
      const sha = repo.refs.get(`refs/heads/${branch}`);
      if (sha === undefined) return NOT_FOUND;
      return { status: 200, body: { name: branch, commit: { sha }, protected: false } };
    }
    return NOT_FOUND;
  }

  if (seg[0] === 'commits' && seg.length === 1 && method === 'GET') {
    const wanted = req.query.get('sha') ?? repo.defaultBranch;
    const tip = resolveRef(repo, wanted);
    if (tip === null) return repo.refs.size === 0 ? { status: 200, body: [] } : NOT_FOUND;
    // Newest first: BFS by distance from the tip.
    const distances = new Map<string, number>();
    let frontier = [tip];
    let distance = 0;
    while (frontier.length > 0) {
      const next: string[] = [];
      for (const oid of frontier) {
        if (distances.has(oid)) continue;
        distances.set(oid, distance);
        const object = objects.get(oid);
        if (object !== undefined && object.type === 'commit') next.push(...parseCommit(object.data).parents);
      }
      frontier = next;
      distance += 1;
    }
    const ordered = [...distances.entries()].sort((a, b) => a[1] - b[1]).map(([oid]) => oid);
    const { page, headers } = paginate(ordered, req.query, `/api/v3/repos/${repo.owner}/${repo.name}/commits`, req.host);
    const body = page.map((oid) => {
      const commit = parseCommit((objects.get(oid) as { data: Buffer }).data);
      const identity = (line: string): Record<string, unknown> => {
        const match = /^(.*?) <(.*?)> (\d+)/.exec(line);
        return {
          name: match?.[1] ?? '',
          email: match?.[2] ?? '',
          date: new Date(Number(match?.[3] ?? 0) * 1000).toISOString(),
        };
      };
      return {
        sha: oid,
        commit: { message: commit.message, author: identity(commit.author), committer: identity(commit.committer) },
        parents: commit.parents.map((parent) => ({ sha: parent })),
      };
    });
    return { status: 200, body, headers };
  }

  if (seg[0] === 'git' && seg[1] === 'refs') {
    if (seg.length === 2 && method === 'POST') {
      const ref = req.body?.ref;
      const sha = req.body?.sha;
      if (typeof ref !== 'string' || !ref.startsWith('refs/')) return message(422, 'Validation Failed: ref is required');
      if (typeof sha !== 'string' || !objects.has(sha)) return message(422, 'Validation Failed: object does not exist');
      if (repo.refs.has(ref)) return message(422, 'Reference already exists');
      repo.refs.set(ref, sha);
      return { status: 201, body: { ref, object: { sha, type: objects.get(sha)?.type ?? 'commit' } } };
    }
    if (seg.length >= 3 && method === 'GET') {
      const tail = seg.slice(2).join('/');
      const full = tail.startsWith('refs/') ? tail : `refs/${tail}`;
      const sha = repo.refs.get(full);
      if (sha === undefined) return NOT_FOUND;
      return { status: 200, body: { ref: full, object: { sha, type: objects.get(sha)?.type ?? 'commit' } } };
    }
    return NOT_FOUND;
  }

  if (seg[0] === 'contents') {
    return handleContents(req, repo, seg.slice(1).join('/'));
  }

  if (seg[0] === 'labels') {
    if (seg.length === 1 && method === 'POST') {
      const name = req.body?.name;
      if (typeof name !== 'string' || name === '') return message(422, 'Validation Failed: name is required');
      if (repo.labels.has(name)) return message(422, 'Validation Failed: label already exists');
      const color = typeof req.body?.color === 'string' ? req.body.color : 'ededed';
      repo.labels.set(name, { name, color });
      return { status: 201, body: labelJson(name, color) };
    }
    if (seg.length === 1 && method === 'GET') {
      return { status: 200, body: [...repo.labels.values()].map((label) => labelJson(label.name, label.color)) };
    }
    return NOT_FOUND;
  }

  if (seg[0] === 'issues') {
    return handleIssues(req, repo, seg.slice(1));
  }

  if (seg[0] === 'pulls') {
    return handlePulls(req, repo, seg.slice(1));
  }

  return NOT_FOUND;
}

// ---------------------------------------------------------------------------

function targetBranchTip(repo: RepoRec, branch: string | undefined): { branch: string; tip: string | null } {
  const name = branch ?? repo.defaultBranch;
  const existing = repo.refs.get(`refs/heads/${name}`);
  if (existing !== undefined) return { branch: name, tip: existing };
  // A contents write naming a new branch forks it from the default branch's tip.
  if (name !== repo.defaultBranch) {
    const baseTip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
    if (baseTip !== undefined) {
      repo.refs.set(`refs/heads/${name}`, baseTip);
      return { branch: name, tip: baseTip };
    }
  }
  return { branch: name, tip: null };
}

function handleContents(req: RestRequest, repo: RepoRec, path: string): RestReply {
  const method = req.method;

  if (method === 'GET') {
    const ref = req.query.get('ref') ?? repo.defaultBranch;
    const commit = resolveRef(repo, ref);
    if (commit === null) return NOT_FOUND;
    let treeOid = treeOfCommit(commit);
    if (path === '') {
      return { status: 200, body: listTree(repo, treeOid, '') };
    }
    const segments = path.split('/');
    for (let i = 0; i < segments.length; i += 1) {
      const object = objects.get(treeOid);
      if (object === undefined || object.type !== 'tree') return NOT_FOUND;
      const entries = parseTree(object.data);
      const entry = entries.find((candidate) => candidate.name === segments[i]);
      if (entry === undefined) return NOT_FOUND;
      if (i === segments.length - 1) {
        if (entry.mode === '40000') return { status: 200, body: listTree(repo, entry.oid, path) };
        const blob = objects.get(entry.oid);
        if (blob === undefined) return NOT_FOUND;
        return {
          status: 200,
          body: {
            type: 'file',
            name: entry.name,
            path,
            sha: entry.oid,
            size: blob.data.length,
            encoding: 'base64',
            content: blob.data.toString('base64'),
          },
        };
      }
      if (entry.mode !== '40000') return NOT_FOUND;
      treeOid = entry.oid;
    }
    return NOT_FOUND;
  }

  if (method === 'PUT') {
    const contentB64 = req.body?.content;
    if (typeof contentB64 !== 'string') return message(422, 'Validation Failed: content is required');
    const content = Buffer.from(contentB64.replace(/[\r\n\t ]+/g, ''), 'base64');
    const commitMessage = typeof req.body?.message === 'string' ? req.body.message : 'Update file';
    const branchField = typeof req.body?.branch === 'string' ? req.body.branch : undefined;
    const { branch, tip } = targetBranchTip(repo, branchField);
    const flat = tip === null ? new Map<string, string>() : flattenTree(treeOfCommit(tip));
    const existing = flat.get(path);
    const shaField = typeof req.body?.sha === 'string' ? req.body.sha : undefined;
    if (existing !== undefined) {
      if (shaField === undefined) return message(422, 'Validation Failed: sha is required when updating');
      if (shaField !== existing) return message(409, 'Conflict: the sha does not match the current file');
    }
    const blobOid = storeObject('blob', content);
    const commit = commitChanges(repo, branch, new Map([[path, content]]), commitMessage);
    return {
      status: existing !== undefined ? 200 : 201,
      body: {
        content: { name: path.split('/').pop() ?? path, path, sha: blobOid, size: content.length, encoding: 'base64' },
        commit: { sha: commit },
      },
    };
  }

  if (method === 'DELETE') {
    const commitMessage = typeof req.body?.message === 'string' ? req.body.message : 'Delete file';
    const branchField = typeof req.body?.branch === 'string' ? req.body.branch : undefined;
    const { branch, tip } = targetBranchTip(repo, branchField);
    if (tip === null) return NOT_FOUND;
    const flat = flattenTree(treeOfCommit(tip));
    const existing = flat.get(path);
    if (existing === undefined) return NOT_FOUND;
    const shaField = typeof req.body?.sha === 'string' ? req.body.sha : undefined;
    if (shaField !== undefined && shaField !== existing) return message(409, 'Conflict: the sha does not match the current file');
    const commit = commitChanges(repo, branch, new Map([[path, null]]), commitMessage);
    return { status: 200, body: { content: null, commit: { sha: commit } } };
  }

  return NOT_FOUND;
}

function listTree(repo: RepoRec, treeOid: string, prefix: string): Array<Record<string, unknown>> {
  void repo;
  const object = objects.get(treeOid);
  if (object === undefined || object.type !== 'tree') return [];
  return parseTree(object.data)
    .map((entry) => {
      const isDir = entry.mode === '40000';
      const blob = isDir ? undefined : objects.get(entry.oid);
      return {
        type: isDir ? 'dir' : 'file',
        name: entry.name,
        path: prefix === '' ? entry.name : `${prefix}/${entry.name}`,
        sha: entry.oid,
        size: blob?.data.length ?? 0,
      };
    })
    .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
}

// ---------------------------------------------------------------------------

function handleIssues(req: RestRequest, repo: RepoRec, seg: string[]): RestReply {
  const method = req.method;

  if (seg.length === 0) {
    if (method === 'POST') {
      const title = req.body?.title;
      if (typeof title !== 'string' || title === '') return message(422, 'Validation Failed: title is required');
      const bodyText = typeof req.body?.body === 'string' ? req.body.body : '';
      const labels = Array.isArray(req.body?.labels) ? req.body.labels.filter((x): x is string => typeof x === 'string') : [];
      for (const label of labels) {
        if (!repo.labels.has(label)) return message(422, `Validation Failed: unknown label ${label}`);
      }
      const issue = createIssue(repo, title, bodyText, labels);
      return { status: 201, body: issueJson(repo, issue) };
    }
    if (method === 'GET') {
      const state = req.query.get('state') ?? 'open';
      const direction = req.query.get('direction') ?? 'desc';
      let list = [...repo.issues.values()];
      if (state === 'open' || state === 'closed') list = list.filter((issue) => issue.state === state);
      list.sort((a, b) => (direction === 'asc' ? a.number - b.number : b.number - a.number));
      const { page, headers } = paginate(list, req.query, `/api/v3/repos/${repo.owner}/${repo.name}/issues`, req.host);
      return { status: 200, body: page.map((issue) => issueJson(repo, issue)), headers };
    }
    return NOT_FOUND;
  }

  const number = Number.parseInt(seg[0], 10);
  const issue = Number.isFinite(number) ? repo.issues.get(number) : undefined;

  if (seg.length === 1) {
    if (issue === undefined) return NOT_FOUND;
    if (method === 'GET') return { status: 200, body: issueJson(repo, issue) };
    if (method === 'PATCH') {
      if (req.body?.state === 'closed' || req.body?.state === 'open') issue.state = req.body.state;
      if (typeof req.body?.title === 'string') issue.title = req.body.title;
      if (typeof req.body?.body === 'string') issue.body = req.body.body;
      if (Array.isArray(req.body?.labels)) {
        const labels = req.body.labels.filter((x): x is string => typeof x === 'string');
        for (const label of labels) {
          if (!repo.labels.has(label)) return message(422, `Validation Failed: unknown label ${label}`);
        }
        issue.labels = labels;
      }
      return { status: 200, body: issueJson(repo, issue) };
    }
    return NOT_FOUND;
  }

  if (seg.length === 2 && seg[1] === 'comments') {
    if (issue === undefined) return NOT_FOUND;
    if (method === 'POST') {
      const bodyText = req.body?.body;
      if (typeof bodyText !== 'string') return message(422, 'Validation Failed: body is required');
      const comment = addComment(issue, bodyText);
      return { status: 201, body: commentJson(comment.body, comment.id, repo.owner) };
    }
    if (method === 'GET') {
      return { status: 200, body: issue.comments.map((comment) => commentJson(comment.body, comment.id, repo.owner)) };
    }
    return NOT_FOUND;
  }

  return NOT_FOUND;
}

// ---------------------------------------------------------------------------

function stripOwnerPrefix(ref: string): string {
  const colon = ref.indexOf(':');
  return colon >= 0 ? ref.slice(colon + 1) : ref;
}

function handlePulls(req: RestRequest, repo: RepoRec, seg: string[]): RestReply {
  const method = req.method;

  if (seg.length === 0) {
    if (method === 'POST') {
      const title = typeof req.body?.title === 'string' ? req.body.title : '';
      const bodyText = typeof req.body?.body === 'string' ? req.body.body : '';
      const head = typeof req.body?.head === 'string' ? stripOwnerPrefix(req.body.head) : '';
      const base = typeof req.body?.base === 'string' ? stripOwnerPrefix(req.body.base) : '';
      const headSha = repo.refs.get(`refs/heads/${head}`);
      const baseSha = repo.refs.get(`refs/heads/${base}`);
      if (headSha === undefined) return message(422, `Validation Failed: head branch ${head} does not exist`);
      if (baseSha === undefined) return message(422, `Validation Failed: base branch ${base} does not exist`);
      for (const pull of repo.pulls.values()) {
        if (pull.state === 'open' && pull.headRef === head && pull.baseRef === base) {
          return message(422, 'Validation Failed: a pull request already exists for this branch combination');
        }
      }
      const pull = createPull(repo, title, bodyText, head, base, headSha, baseSha);
      return { status: 201, body: pullJson(repo, pull) };
    }
    if (method === 'GET') {
      const state = req.query.get('state') ?? 'open';
      let list = [...repo.pulls.values()];
      if (state === 'open') list = list.filter((pull) => pull.state === 'open');
      else if (state === 'closed') list = list.filter((pull) => pull.state === 'closed');
      list.sort((a, b) => a.number - b.number);
      const { page, headers } = paginate(list, req.query, `/api/v3/repos/${repo.owner}/${repo.name}/pulls`, req.host);
      return { status: 200, body: page.map((pull) => pullJson(repo, pull)), headers };
    }
    return NOT_FOUND;
  }

  const number = Number.parseInt(seg[0], 10);
  const pull = Number.isFinite(number) ? repo.pulls.get(number) : undefined;

  if (seg.length === 1 && method === 'GET') {
    if (pull === undefined) return NOT_FOUND;
    return { status: 200, body: pullJson(repo, pull) };
  }

  if (seg.length === 2 && seg[1] === 'files' && method === 'GET') {
    if (pull === undefined) return NOT_FOUND;
    const files = pullChanges(repo, pull).map((change) => ({
      filename: change.path,
      status: change.status,
      additions: change.additions,
      deletions: change.deletions,
      changes: change.additions + change.deletions,
    }));
    return { status: 200, body: files };
  }

  if (seg.length === 2 && seg[1] === 'merge' && method === 'PUT') {
    if (pull === undefined) return NOT_FOUND;
    if (pull.state !== 'open' || pull.merged) return message(405, 'Pull Request is not mergeable');
    const outcome = squashMerge(repo, pull);
    if (!outcome.ok) {
      return message(405, outcome.reason === 'conflict' ? 'Pull Request is not mergeable: merge conflict' : 'Pull Request is not mergeable');
    }
    return { status: 200, body: { sha: outcome.commit, merged: true, message: 'Pull Request successfully merged' } };
  }

  return NOT_FOUND;
}
