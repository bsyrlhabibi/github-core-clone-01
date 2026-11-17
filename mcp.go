package main

// The MCP endpoint at /mcp: JSON-RPC over streamable HTTP. Both protocol
// eras are served — the modern server/discover handshake and the legacy
// initialize handshake — with six tools backed by the same store as REST
// and GraphQL.

import (
	"fmt"
	"strings"
)

var mcpServerInfo = map[string]interface{}{"name": "forge", "version": "1.0.0"}

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type mcpToolDef struct {
	Name        string
	Description string
	Properties  map[string]map[string]string
	PropOrder   []string
	Required    []string
}

func mcpProp(typ, description string) map[string]string {
	return map[string]string{"type": typ, "description": description}
}

var mcpToolsList = []mcpToolDef{
	{
		Name:        "create_repo",
		Description: "Create a repository owned by the authenticated user.",
		Properties: map[string]map[string]string{
			"name":        mcpProp("string", "The repository name."),
			"description": mcpProp("string", "A short description of the repository."),
			"private":     mcpProp("boolean", "Whether the repository is private."),
			"owner":       mcpProp("string", "The repository owner login (defaults to the token owner)."),
		},
		PropOrder: []string{"name", "description", "private", "owner"},
		Required:  []string{"name"},
	},
	{
		Name:        "get_repo",
		Description: "Read one repository by owner and name.",
		Properties: map[string]map[string]string{
			"owner": mcpProp("string", "The repository owner login."),
			"repo":  mcpProp("string", "The repository name."),
		},
		PropOrder: []string{"owner", "repo"},
		Required:  []string{"owner", "repo"},
	},
	{
		Name:        "create_issue",
		Description: "Open an issue in a repository.",
		Properties: map[string]map[string]string{
			"owner": mcpProp("string", "The repository owner login."),
			"repo":  mcpProp("string", "The repository name."),
			"title": mcpProp("string", "The issue title."),
			"body":  mcpProp("string", "The issue body."),
		},
		PropOrder: []string{"owner", "repo", "title", "body"},
		Required:  []string{"owner", "repo", "title"},
	},
	{
		Name:        "list_issues",
		Description: "List the issues of a repository, optionally filtered by state.",
		Properties: map[string]map[string]string{
			"owner": mcpProp("string", "The repository owner login."),
			"repo":  mcpProp("string", "The repository name."),
			"state": mcpProp("string", "Filter by state: open, closed or all."),
		},
		PropOrder: []string{"owner", "repo", "state"},
		Required:  []string{"owner", "repo"},
	},
	{
		Name:        "create_pull_request",
		Description: "Open a pull request from a head branch into a base branch.",
		Properties: map[string]map[string]string{
			"owner": mcpProp("string", "The repository owner login."),
			"repo":  mcpProp("string", "The repository name."),
			"title": mcpProp("string", "The pull request title."),
			"body":  mcpProp("string", "The pull request body."),
			"head":  mcpProp("string", "The head branch name."),
			"base":  mcpProp("string", "The base branch name."),
		},
		PropOrder: []string{"owner", "repo", "title", "body", "head", "base"},
		Required:  []string{"owner", "repo", "title", "head", "base"},
	},
	{
		Name:        "merge_pull_request",
		Description: "Squash-merge a pull request into its base branch.",
		Properties: map[string]map[string]string{
			"owner":       mcpProp("string", "The repository owner login."),
			"repo":        mcpProp("string", "The repository name."),
			"pull_number": mcpProp("number", "The pull request number."),
		},
		PropOrder: []string{"owner", "repo", "pull_number"},
		Required:  []string{"owner", "repo", "pull_number"},
	},
}

var mcpToolsByName = func() map[string]*mcpToolDef {
	out := map[string]*mcpToolDef{}
	for i := range mcpToolsList {
		out[mcpToolsList[i].Name] = &mcpToolsList[i]
	}
	return out
}()

// mcpToolsJSON renders the tool definitions the way tools/list returns
// them: the definition objects themselves.
func mcpToolsJSON() []interface{} {
	out := make([]interface{}, 0, len(mcpToolsList))
	for _, tool := range mcpToolsList {
		properties := map[string]interface{}{}
		for _, key := range tool.PropOrder {
			prop := tool.Properties[key]
			properties[key] = map[string]interface{}{"type": prop["type"], "description": prop["description"]}
		}
		required := make([]interface{}, 0, len(tool.Required))
		for _, name := range tool.Required {
			required = append(required, name)
		}
		out = append(out, map[string]interface{}{
			"name":        tool.Name,
			"description": tool.Description,
			"inputSchema": map[string]interface{}{
				"type":       "object",
				"properties": properties,
				"required":   required,
			},
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// Tool views: the MCP surface has its own compact JSON shapes, distinct
// from the REST shapes.

func mcpRepoView(repo *repoRec) map[string]interface{} {
	return map[string]interface{}{
		"id":             repo.ID,
		"name":           repo.Name,
		"full_name":      repo.Owner + "/" + repo.Name,
		"owner":          map[string]interface{}{"login": repo.Owner},
		"private":        repo.Priv,
		"description":    repo.Description,
		"default_branch": repo.DefaultBranch,
	}
}

func mcpIssueView(repo *repoRec, issue *issueRec) map[string]interface{} {
	labels := make([]interface{}, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		labels = append(labels, map[string]interface{}{"name": name})
	}
	return map[string]interface{}{
		"id":         issue.ID,
		"number":     issue.Number,
		"title":      issue.Title,
		"body":       issue.Body,
		"state":      issue.State,
		"labels":     labels,
		"repository": repo.Owner + "/" + repo.Name,
	}
}

func mcpPullView(repo *repoRec, pull *pullRec) map[string]interface{} {
	headSha, ok := repo.ref("refs/heads/" + pull.HeadRef)
	if !ok {
		headSha = pull.CreatedHeadSha
	}
	baseSha, ok := repo.ref("refs/heads/" + pull.BaseRef)
	if !ok {
		baseSha = pull.CreatedBaseSha
	}
	var mergeCommit interface{}
	if pull.MergeCommitSha != nil {
		mergeCommit = *pull.MergeCommitSha
	}
	return map[string]interface{}{
		"id":               pull.ID,
		"number":           pull.Number,
		"title":            pull.Title,
		"body":             pull.Body,
		"state":            pull.State,
		"merged":           pull.Merged,
		"merge_commit_sha": mergeCommit,
		"head":             map[string]interface{}{"ref": pull.HeadRef, "sha": headSha},
		"base":             map[string]interface{}{"ref": pull.BaseRef, "sha": baseSha},
	}
}

// ---------------------------------------------------------------------------

type toolFailure struct{ message string }

func (e *toolFailure) Error() string { return e.message }

func mcpRequireRepo(owner, name interface{}) *repoRec {
	repo := getRepo(jsString(owner), jsString(name))
	if repo == nil {
		panic(&toolFailure{message: fmt.Sprintf("repository %s/%s not found", jsString(owner), jsString(name))})
	}
	return repo
}

// callMCPTool runs one tool and returns its structured result; tool-level
// failures panic with *toolFailure and are recovered by the caller.
func callMCPTool(name string, args map[string]interface{}) (result map[string]interface{}) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if failure, ok := recovered.(*toolFailure); ok {
				panic(failure)
			}
			panic(recovered)
		}
	}()

	switch name {
	case "create_repo":
		repoName := jsString(args["name"])
		if getRepo(viewer.Login, repoName) != nil {
			panic(&toolFailure{message: "repository " + repoName + " already exists"})
		}
		description, _ := args["description"].(string)
		repo := createRepo(viewer.Login, repoName, description, args["private"] == true)
		return mcpRepoView(repo)

	case "get_repo":
		return mcpRepoView(mcpRequireRepo(args["owner"], args["repo"]))

	case "create_issue":
		repo := mcpRequireRepo(args["owner"], args["repo"])
		body, _ := args["body"].(string)
		issue := createIssue(repo, jsString(args["title"]), body, nil)
		return mcpIssueView(repo, issue)

	case "list_issues":
		repo := mcpRequireRepo(args["owner"], args["repo"])
		state := "open"
		if value, ok := args["state"].(string); ok {
			state = value
		}
		issues := []interface{}{}
		for _, number := range repo.issueOrder {
			issue := repo.issues[number]
			if state == "open" || state == "closed" {
				if issue.State != state {
					continue
				}
			}
			issues = append(issues, mcpIssueView(repo, issue))
		}
		return map[string]interface{}{"issues": issues}

	case "create_pull_request":
		repo := mcpRequireRepo(args["owner"], args["repo"])
		head := jsString(args["head"])
		base := jsString(args["base"])
		headSha, hasHead := repo.ref("refs/heads/" + head)
		if !hasHead {
			panic(&toolFailure{message: "head branch " + head + " does not exist"})
		}
		baseSha, hasBase := repo.ref("refs/heads/" + base)
		if !hasBase {
			panic(&toolFailure{message: "base branch " + base + " does not exist"})
		}
		body, _ := args["body"].(string)
		pull := createPull(repo, jsString(args["title"]), body, head, base, headSha, baseSha)
		return mcpPullView(repo, pull)

	case "merge_pull_request":
		repo := mcpRequireRepo(args["owner"], args["repo"])
		numberValue, _ := jsNumber(args["pull_number"])
		pull := repo.pulls[int(numberValue)]
		if pull == nil {
			panic(&toolFailure{message: fmt.Sprintf("pull request %s not found", jsString(args["pull_number"]))})
		}
		if pull.State == "open" {
			outcome := squashMerge(repo, pull)
			if !outcome.OK {
				panic(&toolFailure{message: "pull request is not mergeable"})
			}
		}
		return mcpPullView(repo, pull)
	}

	panic(&toolFailure{message: "unknown tool " + name})
}

// ---------------------------------------------------------------------------

type mcpReply struct {
	status  int
	body    interface{}
	hasBody bool
}

func mcpRPCResult(id interface{}, result interface{}) mcpReply {
	return mcpReply{status: 200, hasBody: true, body: map[string]interface{}{
		"jsonrpc": "2.0", "id": id, "result": result,
	}}
}

func mcpRPCError(id interface{}, code int, message string, status int) mcpReply {
	return mcpReply{status: status, hasBody: true, body: map[string]interface{}{
		"jsonrpc": "2.0", "id": id, "error": map[string]interface{}{"code": code, "message": message},
	}}
}

// handleMCPOne answers one JSON-RPC message; the second return value is
// false when the message is a notification answered with a bare 202.
func handleMCPOne(message map[string]interface{}) (mcpReply, bool) {
	id := message["id"] // nil when absent, mirroring `message.id ?? null`
	method, _ := message["method"].(string)
	params, _ := message["params"].(map[string]interface{})
	if params == nil {
		params = map[string]interface{}{}
	}
	_, hasID := message["id"]
	isNotification := !hasID

	switch method {
	case "server/discover":
		versions := make([]interface{}, 0, len(mcpSupportedVersions))
		for _, version := range mcpSupportedVersions {
			versions = append(versions, version)
		}
		return mcpRPCResult(id, map[string]interface{}{
			"supportedVersions": versions,
			"capabilities":      map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":        mcpServerInfo,
			"_meta":             map[string]interface{}{"io.modelcontextprotocol/serverInfo": mcpServerInfo},
		}), true

	case "initialize":
		version := "2025-11-25"
		if requested, ok := params["protocolVersion"].(string); ok {
			version = requested
		}
		return mcpRPCResult(id, map[string]interface{}{
			"protocolVersion": version,
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      mcpServerInfo,
		}), true

	case "ping":
		return mcpRPCResult(id, map[string]interface{}{}), true

	case "tools/list":
		return mcpRPCResult(id, map[string]interface{}{"tools": mcpToolsJSON()}), true

	case "tools/call":
		toolName, _ := params["name"].(string)
		tool := mcpToolsByName[toolName]
		if tool == nil {
			return mcpRPCError(id, -32602, "unknown tool: "+toolName, 200), true
		}
		args, _ := params["arguments"].(map[string]interface{})
		if args == nil {
			args = map[string]interface{}{}
		}
		missing := []string{}
		for _, key := range tool.Required {
			value, present := args[key]
			if !present || value == nil || value == "" {
				missing = append(missing, key)
			}
		}
		if len(missing) > 0 {
			return mcpRPCError(id, -32602, "missing required arguments: "+strings.Join(missing, ", "), 200), true
		}
		result, failure := func() (result map[string]interface{}, failure *toolFailure) {
			defer func() {
				if recovered := recover(); recovered != nil {
					if toolErr, ok := recovered.(*toolFailure); ok {
						failure = toolErr
						return
					}
					panic(recovered)
				}
			}()
			result = callMCPTool(toolName, args)
			return result, nil
		}()
		if failure != nil {
			return mcpRPCResult(id, map[string]interface{}{
				"content": []interface{}{map[string]interface{}{"type": "text", "text": failure.message}},
				"isError": true,
			}), true
		}
		text, _ := marshalJSON(result)
		return mcpRPCResult(id, map[string]interface{}{
			"content":           []interface{}{map[string]interface{}{"type": "text", "text": text}},
			"structuredContent": result,
			"isError":           false,
		}), true

	default:
		if isNotification || strings.HasPrefix(method, "notifications/") {
			return mcpReply{}, false // answered 202 with no body
		}
		return mcpRPCError(id, -32601, "method not found: "+method, 200), true
	}
}

// handleMCP serves one POST /mcp: the exact request text in, a status
// and (usually) a JSON-RPC body out. A body that is not JSON is a parse
// error answered HTTP 400 with code -32700.
func handleMCP(rawBody []byte) mcpReply {
	parsed, err := parseJSON(rawBody)
	if err != nil {
		return mcpRPCError(nil, -32700, "Parse error", 400)
	}
	if batch, ok := parsed.([]interface{}); ok {
		replies := []interface{}{}
		for _, entry := range batch {
			entryMap, ok := entry.(map[string]interface{})
			if !ok {
				continue
			}
			reply, respond := handleMCPOne(entryMap)
			if respond && reply.hasBody {
				replies = append(replies, reply.body)
			}
		}
		if len(replies) > 0 {
			return mcpReply{status: 200, hasBody: true, body: replies}
		}
		return mcpReply{status: 202}
	}
	single, ok := parsed.(map[string]interface{})
	if !ok {
		return mcpRPCError(nil, -32600, "Invalid Request", 400)
	}
	reply, respond := handleMCPOne(single)
	if !respond {
		return mcpReply{status: 202}
	}
	return reply
}
