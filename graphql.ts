// GraphQL v4 at /api/graphql: a small parser and executor covering the documents
// gh and github-mcp-server send — viewer, repository fields, __type introspection,
// issue/pull connections with cursor pagination, node(id:), the five mutations,
// operation batching, aliases and inline fragments.

import {
  VIEWER,
  addComment,
  createIssue,
  createPull,
  findIssueById,
  findPullById,
  findRepoById,
  getRepo,
  mergeableState,
  pullChanges,
  squashMerge,
  type IssueRec,
  type PullRec,
  type RepoRec,
} from './store.ts';

// ---------------------------------------------------------------------------
// Global (Relay) ids: base64 of "<version>:<Type><numeric id>", GitHub's shape.

export function encodeGlobalId(type: string, id: number): string {
  return Buffer.from(`04:${type}${id}`, 'utf8').toString('base64');
}

export function decodeGlobalId(gid: string): { type: string; id: number } | null {
  let text: string;
  try {
    text = Buffer.from(gid, 'base64').toString('utf8');
  } catch {
    return null;
  }
  const match = /^\d+:([A-Za-z]+)(\d+)$/.exec(text);
  if (match === null) return null;
  return { type: match[1], id: Number.parseInt(match[2], 10) };
}

// ---------------------------------------------------------------------------
// Tokenizer

interface Token {
  kind: 'name' | 'int' | 'float' | 'string' | 'punct' | 'spread' | 'eof';
  value: string;
}

function tokenize(source: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  while (i < source.length) {
    const ch = source[i];
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === ',' || ch === '﻿') {
      i += 1;
      continue;
    }
    if (ch === '#') {
      while (i < source.length && source[i] !== '\n') i += 1;
      continue;
    }
    if (source.startsWith('...', i)) {
      tokens.push({ kind: 'spread', value: '...' });
      i += 3;
      continue;
    }
    if (ch === '"') {
      if (source.startsWith('"""', i)) {
        const end = source.indexOf('"""', i + 3);
        const text = end < 0 ? source.slice(i + 3) : source.slice(i + 3, end);
        tokens.push({ kind: 'string', value: text });
        i = end < 0 ? source.length : end + 3;
        continue;
      }
      let j = i + 1;
      let out = '';
      while (j < source.length && source[j] !== '"') {
        if (source[j] === '\\' && j + 1 < source.length) {
          const esc = source[j + 1];
          if (esc === 'n') out += '\n';
          else if (esc === 't') out += '\t';
          else if (esc === 'r') out += '\r';
          else if (esc === 'b') out += '\b';
          else if (esc === 'f') out += '\f';
          else if (esc === 'u') {
            out += String.fromCharCode(Number.parseInt(source.slice(j + 2, j + 6), 16));
            j += 4;
          } else out += esc;
          j += 2;
        } else {
          out += source[j];
          j += 1;
        }
      }
      tokens.push({ kind: 'string', value: out });
      i = j + 1;
      continue;
    }
    if (/[0-9-]/.test(ch)) {
      const match = /^-?\d+(\.\d+)?([eE][+-]?\d+)?/.exec(source.slice(i));
      const text = match?.[0] ?? ch;
      tokens.push({ kind: text.includes('.') || /[eE]/.test(text) ? 'float' : 'int', value: text });
      i += text.length;
      continue;
    }
    if (/[_A-Za-z]/.test(ch)) {
      const match = /^[_A-Za-z][_0-9A-Za-z]*/.exec(source.slice(i));
      const text = match?.[0] ?? ch;
      tokens.push({ kind: 'name', value: text });
      i += text.length;
      continue;
    }
    tokens.push({ kind: 'punct', value: ch });
    i += 1;
  }
  tokens.push({ kind: 'eof', value: '' });
  return tokens;
}

// ---------------------------------------------------------------------------
// Parser

type ValueNode =
  | { kind: 'variable'; name: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'string'; value: string }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'list'; values: ValueNode[] }
  | { kind: 'object'; fields: Map<string, ValueNode> };

interface FieldNode {
  alias: string | null;
  name: string;
  args: Map<string, ValueNode>;
  selectionSet: SelectionNode[] | null;
}

type SelectionNode =
  | { kind: 'field'; field: FieldNode }
  | { kind: 'inline'; typeCondition: string | null; selectionSet: SelectionNode[] }
  | { kind: 'spread'; name: string };

interface OperationNode {
  type: 'query' | 'mutation';
  name: string | null;
  selectionSet: SelectionNode[];
}

interface FragmentNode {
  typeCondition: string;
  selectionSet: SelectionNode[];
}

interface DocumentNode {
  operations: OperationNode[];
  fragments: Map<string, FragmentNode>;
}

class Parser {
  tokens: Token[];
  pos: number;
  constructor(tokens: Token[]) {
    this.tokens = tokens;
    this.pos = 0;
  }
  peek(): Token {
    return this.tokens[this.pos];
  }
  next(): Token {
    const token = this.tokens[this.pos];
    this.pos += 1;
    return token;
  }
  expectPunct(value: string): void {
    const token = this.next();
    if (token.kind !== 'punct' || token.value !== value) throw new Error(`expected ${value}, got ${token.value}`);
  }
  skipDirectives(): void {
    while (this.peek().kind === 'punct' && this.peek().value === '@') {
      this.next();
      this.next(); // directive name
      if (this.peek().kind === 'punct' && this.peek().value === '(') this.skipBalancedParens();
    }
  }
  skipBalancedParens(): void {
    let depth = 0;
    for (;;) {
      const token = this.next();
      if (token.kind === 'eof') return;
      if (token.kind === 'punct' && token.value === '(') depth += 1;
      else if (token.kind === 'punct' && token.value === ')') {
        depth -= 1;
        if (depth === 0) return;
      }
    }
  }
  parseDocument(): DocumentNode {
    const operations: OperationNode[] = [];
    const fragments = new Map<string, FragmentNode>();
    while (this.peek().kind !== 'eof') {
      const token = this.peek();
      if (token.kind === 'punct' && token.value === '{') {
        operations.push({ type: 'query', name: null, selectionSet: this.parseSelectionSet() });
        continue;
      }
      if (token.kind === 'name' && (token.value === 'query' || token.value === 'mutation' || token.value === 'subscription')) {
        this.next();
        let name: string | null = null;
        if (this.peek().kind === 'name') name = this.next().value;
        if (this.peek().kind === 'punct' && this.peek().value === '(') this.skipBalancedParens();
        this.skipDirectives();
        operations.push({
          type: token.value === 'mutation' ? 'mutation' : 'query',
          name,
          selectionSet: this.parseSelectionSet(),
        });
        continue;
      }
      if (token.kind === 'name' && token.value === 'fragment') {
        this.next();
        const fragName = this.next().value;
        this.next(); // 'on'
        const typeCondition = this.next().value;
        this.skipDirectives();
        fragments.set(fragName, { typeCondition, selectionSet: this.parseSelectionSet() });
        continue;
      }
      this.next(); // anything else at the top level is skipped
    }
    return { operations, fragments };
  }
  parseSelectionSet(): SelectionNode[] {
    this.expectPunct('{');
    const selections: SelectionNode[] = [];
    while (!(this.peek().kind === 'punct' && this.peek().value === '}')) {
      if (this.peek().kind === 'eof') throw new Error('unterminated selection set');
      if (this.peek().kind === 'spread') {
        this.next();
        if (this.peek().kind === 'name' && this.peek().value === 'on') {
          this.next();
          const typeCondition = this.next().value;
          this.skipDirectives();
          selections.push({ kind: 'inline', typeCondition, selectionSet: this.parseSelectionSet() });
        } else {
          const name = this.next().value;
          this.skipDirectives();
          selections.push({ kind: 'spread', name });
        }
        continue;
      }
      selections.push({ kind: 'field', field: this.parseField() });
    }
    this.next(); // '}'
    return selections;
  }
  parseField(): FieldNode {
    let name = this.next().value;
    let alias: string | null = null;
    if (this.peek().kind === 'punct' && this.peek().value === ':') {
      this.next();
      alias = name;
      name = this.next().value;
    }
    const args = new Map<string, ValueNode>();
    if (this.peek().kind === 'punct' && this.peek().value === '(') {
      this.next();
      while (!(this.peek().kind === 'punct' && this.peek().value === ')')) {
        const argName = this.next().value;
        this.expectPunct(':');
        args.set(argName, this.parseValue());
      }
      this.next(); // ')'
    }
    this.skipDirectives();
    let selectionSet: SelectionNode[] | null = null;
    if (this.peek().kind === 'punct' && this.peek().value === '{') selectionSet = this.parseSelectionSet();
    return { alias, name, args, selectionSet };
  }
  parseValue(): ValueNode {
    const token = this.next();
    if (token.kind === 'punct' && token.value === '$') return { kind: 'variable', name: this.next().value };
    if (token.kind === 'int') return { kind: 'int', value: Number.parseInt(token.value, 10) };
    if (token.kind === 'float') return { kind: 'float', value: Number.parseFloat(token.value) };
    if (token.kind === 'string') return { kind: 'string', value: token.value };
    if (token.kind === 'punct' && token.value === '[') {
      const values: ValueNode[] = [];
      while (!(this.peek().kind === 'punct' && this.peek().value === ']')) values.push(this.parseValue());
      this.next();
      return { kind: 'list', values };
    }
    if (token.kind === 'punct' && token.value === '{') {
      const fields = new Map<string, ValueNode>();
      while (!(this.peek().kind === 'punct' && this.peek().value === '}')) {
        const key = this.next().value;
        this.expectPunct(':');
        fields.set(key, this.parseValue());
      }
      this.next();
      return { kind: 'object', fields };
    }
    if (token.kind === 'name') {
      if (token.value === 'true') return { kind: 'boolean', value: true };
      if (token.value === 'false') return { kind: 'boolean', value: false };
      if (token.value === 'null') return { kind: 'null' };
      return { kind: 'enum', value: token.value };
    }
    throw new Error(`unexpected value token ${token.value}`);
  }
}

function valueOf(node: ValueNode, variables: Record<string, unknown>): unknown {
  switch (node.kind) {
    case 'variable':
      return variables[node.name];
    case 'int':
    case 'float':
    case 'string':
    case 'boolean':
      return node.value;
    case 'null':
      return null;
    case 'enum':
      return node.value;
    case 'list':
      return node.values.map((value) => valueOf(value, variables));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const [key, value] of node.fields) out[key] = valueOf(value, variables);
      return out;
    }
  }
}

// ---------------------------------------------------------------------------
// Schema tables (introspection answers and field validation share them)

const OBJECT_FIELDS: Record<string, string[]> = {
  Query: ['viewer', 'repository', 'node', '__type', '__schema', 'user', 'organization', 'rateLimit'],
  Mutation: ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment', 'updateIssue'],
  User: ['id', 'login', 'name', 'email', 'url', 'avatarUrl', 'databaseId'],
  Repository: [
    'id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission',
    'hasIssuesEnabled', 'issue', 'issues', 'pullRequest', 'pullRequests', 'description', 'isPrivate',
    'createdAt', 'updatedAt', 'url', 'labels', 'milestones', 'viewerCanAdminister',
  ],
  Ref: ['name', 'prefix', 'target'],
  Issue: [
    'id', 'number', 'title', 'state', 'body', 'labels', 'author', 'createdAt', 'updatedAt', 'closedAt',
    'url', 'comments', 'assignees', 'milestone', 'repository', 'databaseId',
  ],
  IssueConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  IssueEdge: ['node', 'cursor'],
  PullRequest: [
    'id', 'number', 'title', 'body', 'state', 'merged', 'mergeable', 'headRefName', 'baseRefName',
    'files', 'author', 'createdAt', 'updatedAt', 'url', 'commits', 'additions', 'deletions',
    'changedFiles', 'databaseId', 'mergeStateStatus', 'isDraft',
  ],
  PullRequestConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  PullRequestEdge: ['node', 'cursor'],
  PullRequestChangedFileConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  PullRequestChangedFile: ['path', 'additions', 'deletions', 'changeType'],
  PageInfo: ['hasNextPage', 'endCursor', 'hasPreviousPage', 'startCursor'],
  IssueComment: ['id', 'body', 'author', 'createdAt', 'updatedAt', 'url'],
  CreateIssuePayload: ['issue', 'clientMutationId'],
  CloseIssuePayload: ['issue', 'clientMutationId'],
  CreatePullRequestPayload: ['pullRequest', 'clientMutationId'],
  MergePullRequestPayload: ['pullRequest', 'clientMutationId'],
  AddCommentPayload: ['commentEdge', 'subject', 'clientMutationId'],
  CommentEdge: ['node', 'cursor'],
  __Type: ['name', 'kind', 'description', 'fields', 'enumValues', 'possibleTypes', 'inputFields', 'specifiedByURL'],
  __Field: ['name', 'description', 'args', 'type', 'isDeprecated', 'deprecationReason'],
  __EnumValue: ['name', 'description', 'isDeprecated', 'deprecationReason'],
  __Schema: ['types', 'queryType', 'mutationType', 'directives'],
};

const ENUM_VALUES: Record<string, string[]> = {
  SearchType: ['ISSUE', 'PULL_REQUEST', 'REPOSITORY', 'USER', 'ORGANIZATION', 'MARKETPLACE', 'DISCUSSION', 'CODE', 'COMMIT', 'TOPIC', 'WIKI'],
  IssueState: ['OPEN', 'CLOSED'],
  PullRequestState: ['OPEN', 'CLOSED', 'MERGED'],
  MergeableState: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'],
  RepositoryPermission: ['ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'],
  IssueOrderField: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'],
  OrderDirection: ['ASC', 'DESC'],
  PullRequestMergeMethod: ['MERGE', 'SQUASH', 'REBASE'],
  MergeStateStatus: ['CLEAN', 'DIRTY', 'BLOCKED', 'BEHIND', 'UNSTABLE', 'UNKNOWN'],
};

function typeKind(name: string): string | null {
  if (OBJECT_FIELDS[name] !== undefined) return 'OBJECT';
  if (ENUM_VALUES[name] !== undefined) return 'ENUM';
  if (name === 'Node') return 'INTERFACE';
  if (name === 'String' || name === 'Int' || name === 'Boolean' || name === 'ID' || name === 'Float') return 'SCALAR';
  return null;
}

// ---------------------------------------------------------------------------
// Executor

/** A resolved object: `__t` is its GraphQL type; the rest is resolver data. */
type Source = Record<string, unknown> & { __t: string };

interface ExecContext {
  variables: Record<string, unknown>;
  fragments: Map<string, FragmentNode>;
  errors: Array<{ message: string }>;
}

class FieldError extends Error {}

function repoSource(repo: RepoRec): Source {
  return { __t: 'Repository', repo };
}
function issueSource(repo: RepoRec, issue: IssueRec): Source {
  return { __t: 'Issue', repo, issue };
}
function pullSource(repo: RepoRec, pull: PullRec): Source {
  return { __t: 'PullRequest', repo, pull };
}

function cursorFor(number: number): string {
  return Buffer.from(`issue-cursor:${number}`, 'utf8').toString('base64');
}

function resolveField(source: Source, field: FieldNode, ctx: ExecContext): unknown {
  const type = source.__t;
  const args = new Map<string, unknown>();
  for (const [key, value] of field.args) args.set(key, valueOf(value, ctx.variables));
  const repo = source.repo as RepoRec | undefined;

  switch (type) {
    case 'Query': {
      if (field.name === 'viewer') return { __t: 'User', login: VIEWER.login };
      if (field.name === 'repository') {
        const found = getRepo(String(args.get('owner') ?? ''), String(args.get('name') ?? ''));
        return found === null ? null : repoSource(found);
      }
      if (field.name === 'node') {
        const decoded = decodeGlobalId(String(args.get('id') ?? ''));
        if (decoded === null) return null;
        if (decoded.type === 'Repository') {
          const found = findRepoById(decoded.id);
          return found === null ? null : repoSource(found);
        }
        if (decoded.type === 'Issue') {
          const found = findIssueById(decoded.id);
          return found === null ? null : issueSource(found.repo, found.issue);
        }
        if (decoded.type === 'PullRequest') {
          const found = findPullById(decoded.id);
          return found === null ? null : pullSource(found.repo, found.pull);
        }
        return null;
      }
      if (field.name === '__type') {
        const name = String(args.get('name') ?? '');
        return typeKind(name) === null ? null : { __t: '__Type', typeName: name };
      }
      if (field.name === '__schema') return { __t: '__Schema' };
      return null;
    }
    case 'Mutation': {
      if (field.name === 'createIssue') {
        const input = (args.get('input') ?? {}) as Record<string, unknown>;
        const decoded = decodeGlobalId(String(input.repositoryId ?? ''));
        const target = decoded !== null && decoded.type === 'Repository' ? findRepoById(decoded.id) : null;
        if (target === null) throw new FieldError('repository not found for createIssue');
        const issue = createIssue(target, String(input.title ?? ''), typeof input.body === 'string' ? input.body : '', []);
        return { __t: 'CreateIssuePayload', issueDesc: issueSource(target, issue) };
      }
      if (field.name === 'closeIssue') {
        const input = (args.get('input') ?? {}) as Record<string, unknown>;
        const decoded = decodeGlobalId(String(input.issueId ?? ''));
        const found = decoded !== null && decoded.type === 'Issue' ? findIssueById(decoded.id) : null;
        if (found === null) throw new FieldError('issue not found for closeIssue');
        found.issue.state = 'closed';
        return { __t: 'CloseIssuePayload', issueDesc: issueSource(found.repo, found.issue) };
      }
      if (field.name === 'createPullRequest') {
        const input = (args.get('input') ?? {}) as Record<string, unknown>;
        const decoded = decodeGlobalId(String(input.repositoryId ?? ''));
        const target = decoded !== null && decoded.type === 'Repository' ? findRepoById(decoded.id) : null;
        if (target === null) throw new FieldError('repository not found for createPullRequest');
        const head = String(input.headRefName ?? '');
        const base = String(input.baseRefName ?? '');
        const headSha = target.refs.get(`refs/heads/${head}`);
        const baseSha = target.refs.get(`refs/heads/${base}`);
        if (headSha === undefined || baseSha === undefined) throw new FieldError('head or base branch does not exist');
        const pull = createPull(
          target,
          String(input.title ?? ''),
          typeof input.body === 'string' ? input.body : '',
          head,
          base,
          headSha,
          baseSha,
        );
        return { __t: 'CreatePullRequestPayload', pullDesc: pullSource(target, pull) };
      }
      if (field.name === 'mergePullRequest') {
        const input = (args.get('input') ?? {}) as Record<string, unknown>;
        const decoded = decodeGlobalId(String(input.pullRequestId ?? ''));
        const found = decoded !== null && decoded.type === 'PullRequest' ? findPullById(decoded.id) : null;
        if (found === null) throw new FieldError('pull request not found for mergePullRequest');
        if (found.pull.state === 'open') {
          const outcome = squashMerge(found.repo, found.pull);
          if (!outcome.ok) throw new FieldError('Pull Request is not mergeable');
        }
        return { __t: 'MergePullRequestPayload', pullDesc: pullSource(found.repo, found.pull) };
      }
      if (field.name === 'addComment') {
        const input = (args.get('input') ?? {}) as Record<string, unknown>;
        const decoded = decodeGlobalId(String(input.subjectId ?? ''));
        const found = decoded !== null && decoded.type === 'Issue' ? findIssueById(decoded.id) : null;
        if (found === null) throw new FieldError('subject not found for addComment');
        const comment = addComment(found.issue, String(input.body ?? ''));
        return {
          __t: 'AddCommentPayload',
          comment: { id: comment.id, body: comment.body },
          issueDesc: issueSource(found.repo, found.issue),
        };
      }
      return null;
    }
    case 'User': {
      if (field.name === 'login') return source.login as string;
      if (field.name === 'id') return encodeGlobalId('User', VIEWER.id);
      if (field.name === 'name') return VIEWER.name;
      if (field.name === 'email') return VIEWER.email;
      return null;
    }
    case 'Repository': {
      const record = repo as RepoRec;
      switch (field.name) {
        case 'id':
          return encodeGlobalId('Repository', record.id);
        case 'databaseId':
          return record.id;
        case 'name':
          return record.name;
        case 'nameWithOwner':
          return `${record.owner}/${record.name}`;
        case 'owner':
          return { __t: 'User', login: record.owner };
        case 'defaultBranchRef': {
          const ref = `refs/heads/${record.defaultBranch}`;
          return record.refs.has(ref) ? { __t: 'Ref', name: record.defaultBranch } : null;
        }
        case 'viewerPermission':
          return 'ADMIN';
        case 'hasIssuesEnabled':
          return true;
        case 'description':
          return record.description;
        case 'isPrivate':
          return record.priv;
        case 'issue': {
          const issue = record.issues.get(Number(args.get('number')));
          return issue === undefined ? null : issueSource(record, issue);
        }
        case 'issues': {
          const states = args.get('states');
          let list = [...record.issues.values()];
          if (Array.isArray(states)) {
            const wanted = new Set(states.map((state) => String(state).toLowerCase()));
            list = list.filter((issue) => wanted.has(issue.state));
          }
          const orderBy = args.get('orderBy') as Record<string, unknown> | undefined;
          const direction = typeof orderBy?.direction === 'string' ? orderBy.direction : null;
          list.sort((a, b) => (direction === 'DESC' ? b.number - a.number : a.number - b.number));
          return {
            __t: 'IssueConnection',
            repo: record,
            all: list,
            first: typeof args.get('first') === 'number' ? (args.get('first') as number) : 30,
            after: typeof args.get('after') === 'string' ? (args.get('after') as string) : null,
          };
        }
        case 'pullRequest': {
          const pull = record.pulls.get(Number(args.get('number')));
          return pull === undefined ? null : pullSource(record, pull);
        }
        case 'pullRequests': {
          const open = [...record.pulls.values()].filter((pull) => pull.state === 'open');
          open.sort((a, b) => a.number - b.number);
          return {
            __t: 'PullRequestConnection',
            repo: record,
            all: open,
            first: typeof args.get('first') === 'number' ? (args.get('first') as number) : 30,
          };
        }
        default:
          return null;
      }
    }
    case 'Ref': {
      if (field.name === 'name') return source.name as string;
      return null;
    }
    case 'Issue': {
      const issue = source.issue as IssueRec;
      switch (field.name) {
        case 'id':
          return encodeGlobalId('Issue', issue.id);
        case 'databaseId':
          return issue.id;
        case 'number':
          return issue.number;
        case 'title':
          return issue.title;
        case 'body':
          return issue.body;
        case 'state':
          return issue.state.toUpperCase();
        default:
          return null;
      }
    }
    case 'IssueConnection': {
      const all = source.all as IssueRec[];
      const first = source.first as number;
      const after = source.after as string | null;
      let start = 0;
      if (after !== null) {
        try {
          const decoded = Buffer.from(after, 'base64').toString('utf8');
          const number = Number.parseInt(decoded.replace('issue-cursor:', ''), 10);
          const index = all.findIndex((issue) => issue.number === number);
          if (index >= 0) start = index + 1;
        } catch {
          start = 0;
        }
      }
      const slice = all.slice(start, start + first);
      if (field.name === 'nodes') return slice.map((issue) => issueSource(repo as RepoRec, issue));
      if (field.name === 'edges') {
        return slice.map((issue) => ({ __t: 'IssueEdge', repo, issue, cursor: cursorFor(issue.number) }));
      }
      if (field.name === 'pageInfo') {
        return {
          __t: 'PageInfo',
          hasNextPage: start + slice.length < all.length,
          endCursor: slice.length > 0 ? cursorFor(slice[slice.length - 1].number) : null,
        };
      }
      if (field.name === 'totalCount') return all.length;
      return null;
    }
    case 'IssueEdge': {
      if (field.name === 'node') return issueSource(repo as RepoRec, source.issue as IssueRec);
      if (field.name === 'cursor') return source.cursor as string;
      return null;
    }
    case 'PullRequest': {
      const pull = source.pull as PullRec;
      const record = repo as RepoRec;
      switch (field.name) {
        case 'id':
          return encodeGlobalId('PullRequest', pull.id);
        case 'databaseId':
          return pull.id;
        case 'number':
          return pull.number;
        case 'title':
          return pull.title;
        case 'body':
          return pull.body;
        case 'state':
          return pull.merged ? 'MERGED' : pull.state.toUpperCase();
        case 'merged':
          return pull.merged;
        case 'mergeable':
          return mergeableState(record, pull);
        case 'headRefName':
          return pull.headRef;
        case 'baseRefName':
          return pull.baseRef;
        case 'files': {
          const changes = pullChanges(record, pull);
          return {
            __t: 'PullRequestChangedFileConnection',
            changes,
            first: typeof args.get('first') === 'number' ? (args.get('first') as number) : 30,
          };
        }
        default:
          return null;
      }
    }
    case 'PullRequestConnection': {
      const all = source.all as PullRec[];
      const first = source.first as number;
      const slice = all.slice(0, first);
      if (field.name === 'nodes') return slice.map((pull) => pullSource(repo as RepoRec, pull));
      if (field.name === 'pageInfo') return { __t: 'PageInfo', hasNextPage: all.length > slice.length, endCursor: null };
      if (field.name === 'totalCount') return all.length;
      return null;
    }
    case 'PullRequestChangedFileConnection': {
      const changes = source.changes as ReturnType<typeof pullChanges>;
      const first = source.first as number;
      const slice = changes.slice(0, first);
      if (field.name === 'nodes') return slice.map((change) => ({ __t: 'PullRequestChangedFile', change }));
      if (field.name === 'totalCount') return changes.length;
      if (field.name === 'pageInfo') return { __t: 'PageInfo', hasNextPage: changes.length > slice.length, endCursor: null };
      return null;
    }
    case 'PullRequestChangedFile': {
      const change = source.change as { path: string; status: string; additions: number; deletions: number };
      if (field.name === 'path') return change.path;
      if (field.name === 'additions') return change.additions;
      if (field.name === 'deletions') return change.deletions;
      if (field.name === 'changeType') return change.status === 'removed' ? 'DELETED' : change.status.toUpperCase();
      return null;
    }
    case 'PageInfo': {
      if (field.name === 'hasNextPage') return source.hasNextPage as boolean;
      if (field.name === 'endCursor') return (source.endCursor as string | null) ?? null;
      if (field.name === 'hasPreviousPage') return false;
      if (field.name === 'startCursor') return (source.endCursor as string | null) ?? null;
      return null;
    }
    case 'CreateIssuePayload':
    case 'CloseIssuePayload': {
      if (field.name === 'issue') return source.issueDesc as Source;
      return null;
    }
    case 'CreatePullRequestPayload':
    case 'MergePullRequestPayload': {
      if (field.name === 'pullRequest') return source.pullDesc as Source;
      return null;
    }
    case 'AddCommentPayload': {
      if (field.name === 'commentEdge') {
        const comment = source.comment as { id: number; body: string };
        return { __t: 'CommentEdge', comment };
      }
      if (field.name === 'subject') return source.issueDesc as Source;
      return null;
    }
    case 'CommentEdge': {
      const comment = source.comment as { id: number; body: string };
      if (field.name === 'node') return { __t: 'IssueComment', comment };
      if (field.name === 'cursor') return cursorFor(comment.id);
      return null;
    }
    case 'IssueComment': {
      const comment = source.comment as { id: number; body: string };
      if (field.name === 'id') return encodeGlobalId('IssueComment', comment.id);
      if (field.name === 'body') return comment.body;
      return null;
    }
    case '__Type': {
      const name = source.typeName as string;
      if (field.name === 'name') return name;
      if (field.name === 'kind') return typeKind(name);
      if (field.name === 'fields') return (OBJECT_FIELDS[name] ?? []).map((fieldName) => ({ __t: '__Field', fieldName }));
      if (field.name === 'enumValues') return (ENUM_VALUES[name] ?? []).map((value) => ({ __t: '__EnumValue', valueName: value }));
      return null;
    }
    case '__Field': {
      if (field.name === 'name') return source.fieldName as string;
      return null;
    }
    case '__EnumValue': {
      if (field.name === 'name') return source.valueName as string;
      return null;
    }
    default:
      return null;
  }
}

function applySelectionSet(source: Source, selections: SelectionNode[], ctx: ExecContext): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  const visit = (set: SelectionNode[]): void => {
    for (const selection of set) {
      if (selection.kind === 'inline') {
        if (selection.typeCondition === null || selection.typeCondition === source.__t || selection.typeCondition === 'Node') {
          visit(selection.selectionSet);
        }
        continue;
      }
      if (selection.kind === 'spread') {
        const fragment = ctx.fragments.get(selection.name);
        if (fragment !== undefined && (fragment.typeCondition === source.__t || fragment.typeCondition === 'Node')) {
          visit(fragment.selectionSet);
        }
        continue;
      }
      const field = selection.field;
      const key = field.alias ?? field.name;
      if (field.name === '__typename') {
        out[key] = source.__t;
        continue;
      }
      const known = (OBJECT_FIELDS[source.__t] ?? []).includes(field.name) || source.__t === 'Node';
      if (!known) {
        ctx.errors.push({ message: `Field '${field.name}' doesn't exist on type '${source.__t}'` });
        out[key] = null;
        continue;
      }
      let value: unknown;
      try {
        value = resolveField(source, field, ctx);
      } catch (err) {
        if (err instanceof FieldError) {
          ctx.errors.push({ message: err.message });
          out[key] = null;
          continue;
        }
        throw err;
      }
      if (field.selectionSet !== null && value !== null && value !== undefined) {
        if (Array.isArray(value)) {
          out[key] = value.map((item) =>
            item !== null && typeof item === 'object' && '__t' in (item as Source)
              ? applySelectionSet(item as Source, field.selectionSet as SelectionNode[], ctx)
              : item,
          );
        } else if (typeof value === 'object' && '__t' in (value as Source)) {
          out[key] = applySelectionSet(value as Source, field.selectionSet, ctx);
        } else {
          out[key] = value;
        }
      } else {
        out[key] = value ?? null;
      }
    }
  };
  visit(selections);
  return out;
}

// ---------------------------------------------------------------------------
// Entry point

export interface GraphQLReply {
  status: number;
  body: unknown;
}

function executeOperation(document: DocumentNode, operationName: string | null, variables: Record<string, unknown>): GraphQLReply {
  let operation = document.operations[0];
  if (operationName !== null) {
    operation = document.operations.find((candidate) => candidate.name === operationName) ?? operation;
  }
  if (operation === undefined) {
    return { status: 200, body: { data: null, errors: [{ message: 'no operation in the document' }] } };
  }
  const ctx: ExecContext = { variables, fragments: document.fragments, errors: [] };
  const root: Source = { __t: operation.type === 'mutation' ? 'Mutation' : 'Query' };
  const data = applySelectionSet(root, operation.selectionSet, ctx);
  const body: Record<string, unknown> = { data };
  if (ctx.errors.length > 0) body.errors = ctx.errors;
  return { status: 200, body };
}

function executeOne(payload: Record<string, unknown>): GraphQLReply {
  const query = payload.query;
  if (typeof query !== 'string' || query === '') {
    return { status: 400, body: { errors: [{ message: 'a GraphQL query string is required' }] } };
  }
  const variables = (payload.variables ?? {}) as Record<string, unknown>;
  const operationName = typeof payload.operationName === 'string' ? payload.operationName : null;
  let document: DocumentNode;
  try {
    document = new Parser(tokenize(query)).parseDocument();
  } catch (err) {
    return { status: 200, body: { data: null, errors: [{ message: err instanceof Error ? err.message : 'parse error' }] } };
  }
  return executeOperation(document, operationName, variables);
}

/** Handle one POST /api/graphql body (already JSON-parsed): a single operation or a batch array. */
export function handleGraphQL(body: unknown): GraphQLReply {
  if (Array.isArray(body)) {
    const results = body.map((entry) => executeOne((entry ?? {}) as Record<string, unknown>).body);
    return { status: 200, body: results };
  }
  if (body !== null && typeof body === 'object') return executeOne(body as Record<string, unknown>);
  return { status: 400, body: { errors: [{ message: 'a GraphQL request body is required' }] } };
}
