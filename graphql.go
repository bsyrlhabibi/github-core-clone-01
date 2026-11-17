package main

// GraphQL v4 at /api/graphql: a small parser and executor covering the
// documents gh and github-mcp-server send — viewer, repository fields,
// __type introspection, issue/pull connections with cursor pagination,
// node(id:), the five mutations, operation batching, aliases and inline
// fragments.

import (
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Global (Relay) ids: base64 of "<version>:<Type><numeric id>", GitHub's
// shape.

func encodeGlobalID(typeName string, id int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("04:%s%d", typeName, id)))
}

var globalIDPattern = regexp.MustCompile(`^\d+:([A-Za-z]+)(\d+)$`)

func decodeGlobalID(gid string) (string, int, bool) {
	text := lenientBase64ToString(gid)
	match := globalIDPattern.FindStringSubmatch(text)
	if match == nil {
		return "", 0, false
	}
	id, _ := strconv.Atoi(match[2])
	return match[1], id, true
}

// ---------------------------------------------------------------------------
// Tokenizer

const (
	tokenName = iota
	tokenInt
	tokenFloat
	tokenString
	tokenPunct
	tokenSpread
	tokenEOF
)

type gqlToken struct {
	kind  int
	value string
}

var (
	gqlNumberPattern = regexp.MustCompile(`^-?\d+(\.\d+)?([eE][+-]?\d+)?`)
	gqlNamePattern   = regexp.MustCompile(`^[_A-Za-z][_0-9A-Za-z]*`)
)

func isDigitOrMinus(c byte) bool {
	return c >= '0' && c <= '9' || c == '-'
}

func isNameStart(c byte) bool {
	return c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

func tokenizeGraphQL(source string) []gqlToken {
	tokens := []gqlToken{}
	i := 0
	for i < len(source) {
		ch := source[i]
		if i+3 <= len(source) && source[i] == 0xEF && source[i+1] == 0xBB && source[i+2] == 0xBF {
			i += 3 // UTF-8 byte order mark
			continue
		}
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' {
			i++
			continue
		}
		if ch == '#' {
			for i < len(source) && source[i] != '\n' {
				i++
			}
			continue
		}
		if strings.HasPrefix(source[i:], "...") {
			tokens = append(tokens, gqlToken{kind: tokenSpread, value: "..."})
			i += 3
			continue
		}
		if ch == '"' {
			if strings.HasPrefix(source[i:], "\"\"\"") {
				end := strings.Index(source[i+3:], "\"\"\"")
				var text string
				if end < 0 {
					text = source[i+3:]
					i = len(source)
				} else {
					text = source[i+3 : i+3+end]
					i = i + 3 + end + 3
				}
				tokens = append(tokens, gqlToken{kind: tokenString, value: text})
				continue
			}
			j := i + 1
			var out strings.Builder
			for j < len(source) && source[j] != '"' {
				if source[j] == '\\' && j+1 < len(source) {
					esc := source[j+1]
					switch esc {
					case 'n':
						out.WriteByte('\n')
					case 't':
						out.WriteByte('\t')
					case 'r':
						out.WriteByte('\r')
					case 'b':
						out.WriteByte('\b')
					case 'f':
						out.WriteByte('\f')
					case 'u':
						codeText := ""
						if j+6 <= len(source) {
							codeText = source[j+2 : j+6]
						} else {
							codeText = source[j+2:]
						}
						code, _ := strconv.ParseInt(codeText, 16, 32)
						out.WriteRune(rune(code))
						j += 4
					default:
						out.WriteByte(esc)
					}
					j += 2
				} else {
					out.WriteByte(source[j])
					j++
				}
			}
			tokens = append(tokens, gqlToken{kind: tokenString, value: out.String()})
			i = j + 1
			continue
		}
		if isDigitOrMinus(ch) {
			match := gqlNumberPattern.FindString(source[i:])
			text := match
			if text == "" {
				text = string(ch)
			}
			kind := tokenInt
			if strings.ContainsAny(text, ".eE") {
				kind = tokenFloat
			}
			tokens = append(tokens, gqlToken{kind: kind, value: text})
			i += len(text)
			continue
		}
		if isNameStart(ch) {
			match := gqlNamePattern.FindString(source[i:])
			text := match
			if text == "" {
				text = string(ch)
			}
			tokens = append(tokens, gqlToken{kind: tokenName, value: text})
			i += len(text)
			continue
		}
		tokens = append(tokens, gqlToken{kind: tokenPunct, value: string(ch)})
		i++
	}
	tokens = append(tokens, gqlToken{kind: tokenEOF, value: ""})
	return tokens
}

// ---------------------------------------------------------------------------
// Parser

const (
	valueVariable = iota
	valueInt
	valueFloat
	valueString
	valueBoolean
	valueNull
	valueEnum
	valueList
	valueObject
)

type valueNode struct {
	kind     int
	name     string // variable name
	intVal   int64
	floatVal float64
	strVal   string
	boolVal  bool
	values   []*valueNode
	fields   map[string]*valueNode
}

type fieldNode struct {
	alias        string
	hasAlias     bool
	name         string
	args         map[string]*valueNode
	argOrder     []string
	selectionSet []selectionNode
	hasSelection bool
}

const (
	selectionField = iota
	selectionInline
	selectionSpread
)

type selectionNode struct {
	kind          int
	field         *fieldNode
	typeCondition string
	hasCondition  bool
	selectionSet  []selectionNode
	spreadName    string
}

type operationNode struct {
	opType       string // "query" | "mutation"
	name         string
	hasName      bool
	selectionSet []selectionNode
}

type fragmentNode struct {
	typeCondition string
	selectionSet  []selectionNode
}

type documentNode struct {
	operations []operationNode
	fragments  map[string]*fragmentNode
}

type gqlParser struct {
	tokens []gqlToken
	pos    int
}

func (p *gqlParser) peek() gqlToken {
	return p.tokens[p.pos]
}

func (p *gqlParser) next() gqlToken {
	token := p.tokens[p.pos]
	p.pos++
	return token
}

func (p *gqlParser) expectPunct(value string) error {
	token := p.next()
	if token.kind != tokenPunct || token.value != value {
		return fmt.Errorf("expected %s, got %s", value, token.value)
	}
	return nil
}

func (p *gqlParser) skipDirectives() {
	for p.peek().kind == tokenPunct && p.peek().value == "@" {
		p.next()
		p.next() // directive name
		if p.peek().kind == tokenPunct && p.peek().value == "(" {
			p.skipBalancedParens()
		}
	}
}

func (p *gqlParser) skipBalancedParens() {
	depth := 0
	for {
		token := p.next()
		if token.kind == tokenEOF {
			return
		}
		if token.kind == tokenPunct && token.value == "(" {
			depth++
		} else if token.kind == tokenPunct && token.value == ")" {
			depth--
			if depth == 0 {
				return
			}
		}
	}
}

func (p *gqlParser) parseDocument() (*documentNode, error) {
	document := &documentNode{fragments: map[string]*fragmentNode{}}
	for p.peek().kind != tokenEOF {
		token := p.peek()
		if token.kind == tokenPunct && token.value == "{" {
			selectionSet, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			document.operations = append(document.operations, operationNode{opType: "query", selectionSet: selectionSet})
			continue
		}
		if token.kind == tokenName && (token.value == "query" || token.value == "mutation" || token.value == "subscription") {
			p.next()
			op := operationNode{opType: "query"}
			if token.value == "mutation" {
				op.opType = "mutation"
			}
			if p.peek().kind == tokenName {
				op.name = p.next().value
				op.hasName = true
			}
			if p.peek().kind == tokenPunct && p.peek().value == "(" {
				p.skipBalancedParens()
			}
			p.skipDirectives()
			selectionSet, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			op.selectionSet = selectionSet
			document.operations = append(document.operations, op)
			continue
		}
		if token.kind == tokenName && token.value == "fragment" {
			p.next()
			fragName := p.next().value
			p.next() // 'on'
			typeCondition := p.next().value
			p.skipDirectives()
			selectionSet, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			document.fragments[fragName] = &fragmentNode{typeCondition: typeCondition, selectionSet: selectionSet}
			continue
		}
		p.next() // anything else at the top level is skipped
	}
	return document, nil
}

func (p *gqlParser) parseSelectionSet() ([]selectionNode, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	selections := []selectionNode{}
	for !(p.peek().kind == tokenPunct && p.peek().value == "}") {
		if p.peek().kind == tokenEOF {
			return nil, errors.New("unterminated selection set")
		}
		if p.peek().kind == tokenSpread {
			p.next()
			if p.peek().kind == tokenName && p.peek().value == "on" {
				p.next()
				typeCondition := p.next().value
				p.skipDirectives()
				selectionSet, err := p.parseSelectionSet()
				if err != nil {
					return nil, err
				}
				selections = append(selections, selectionNode{
					kind: selectionInline, typeCondition: typeCondition, hasCondition: true, selectionSet: selectionSet,
				})
			} else {
				name := p.next().value
				p.skipDirectives()
				selections = append(selections, selectionNode{kind: selectionSpread, spreadName: name})
			}
			continue
		}
		field, err := p.parseField()
		if err != nil {
			return nil, err
		}
		selections = append(selections, selectionNode{kind: selectionField, field: field})
	}
	p.next() // '}'
	return selections, nil
}

func (p *gqlParser) parseField() (*fieldNode, error) {
	name := p.next().value
	field := &fieldNode{name: name, args: map[string]*valueNode{}}
	if p.peek().kind == tokenPunct && p.peek().value == ":" {
		p.next()
		field.alias = name
		field.hasAlias = true
		field.name = p.next().value
	}
	if p.peek().kind == tokenPunct && p.peek().value == "(" {
		p.next()
		for !(p.peek().kind == tokenPunct && p.peek().value == ")") {
			argName := p.next().value
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			value, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			if _, exists := field.args[argName]; !exists {
				field.argOrder = append(field.argOrder, argName)
			}
			field.args[argName] = value
		}
		p.next() // ')'
	}
	p.skipDirectives()
	if p.peek().kind == tokenPunct && p.peek().value == "{" {
		selectionSet, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		field.selectionSet = selectionSet
		field.hasSelection = true
	}
	return field, nil
}

func (p *gqlParser) parseValue() (*valueNode, error) {
	token := p.next()
	if token.kind == tokenPunct && token.value == "$" {
		return &valueNode{kind: valueVariable, name: p.next().value}, nil
	}
	if token.kind == tokenInt {
		value, _ := strconv.ParseInt(token.value, 10, 64)
		return &valueNode{kind: valueInt, intVal: value}, nil
	}
	if token.kind == tokenFloat {
		value, _ := strconv.ParseFloat(token.value, 64)
		return &valueNode{kind: valueFloat, floatVal: value}, nil
	}
	if token.kind == tokenString {
		return &valueNode{kind: valueString, strVal: token.value}, nil
	}
	if token.kind == tokenPunct && token.value == "[" {
		node := &valueNode{kind: valueList, values: []*valueNode{}}
		for !(p.peek().kind == tokenPunct && p.peek().value == "]") {
			value, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			node.values = append(node.values, value)
		}
		p.next()
		return node, nil
	}
	if token.kind == tokenPunct && token.value == "{" {
		node := &valueNode{kind: valueObject, fields: map[string]*valueNode{}}
		for !(p.peek().kind == tokenPunct && p.peek().value == "}") {
			key := p.next().value
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			value, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			node.fields[key] = value
		}
		p.next()
		return node, nil
	}
	if token.kind == tokenName {
		switch token.value {
		case "true":
			return &valueNode{kind: valueBoolean, boolVal: true}, nil
		case "false":
			return &valueNode{kind: valueBoolean, boolVal: false}, nil
		case "null":
			return &valueNode{kind: valueNull}, nil
		default:
			return &valueNode{kind: valueEnum, strVal: token.value}, nil
		}
	}
	return nil, fmt.Errorf("unexpected value token %s", token.value)
}

func valueOf(node *valueNode, variables map[string]interface{}) interface{} {
	switch node.kind {
	case valueVariable:
		return variables[node.name]
	case valueInt:
		return float64(node.intVal)
	case valueFloat:
		return node.floatVal
	case valueString:
		return node.strVal
	case valueBoolean:
		return node.boolVal
	case valueNull:
		return nil
	case valueEnum:
		return node.strVal
	case valueList:
		out := make([]interface{}, 0, len(node.values))
		for _, value := range node.values {
			out = append(out, valueOf(value, variables))
		}
		return out
	case valueObject:
		out := map[string]interface{}{}
		for key, value := range node.fields {
			out[key] = valueOf(value, variables)
		}
		return out
	}
	return nil
}

// ---------------------------------------------------------------------------
// Schema tables (introspection answers and field validation share them)

var objectFields = map[string][]string{
	"Query":    {"viewer", "repository", "node", "__type", "__schema", "user", "organization", "rateLimit"},
	"Mutation": {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment", "updateIssue"},
	"User":     {"id", "login", "name", "email", "url", "avatarUrl", "databaseId"},
	"Repository": {
		"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission",
		"hasIssuesEnabled", "issue", "issues", "pullRequest", "pullRequests", "description", "isPrivate",
		"createdAt", "updatedAt", "url", "labels", "milestones", "viewerCanAdminister",
	},
	"Ref": {"name", "prefix", "target"},
	"Issue": {
		"id", "number", "title", "state", "body", "labels", "author", "createdAt", "updatedAt", "closedAt",
		"url", "comments", "assignees", "milestone", "repository", "databaseId",
	},
	"IssueConnection": {"nodes", "edges", "pageInfo", "totalCount"},
	"IssueEdge":       {"node", "cursor"},
	"PullRequest": {
		"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName",
		"files", "author", "createdAt", "updatedAt", "url", "commits", "additions", "deletions",
		"changedFiles", "databaseId", "mergeStateStatus", "isDraft",
	},
	"PullRequestConnection":            {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestEdge":                  {"node", "cursor"},
	"PullRequestChangedFileConnection": {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestChangedFile":           {"path", "additions", "deletions", "changeType"},
	"PageInfo":                         {"hasNextPage", "endCursor", "hasPreviousPage", "startCursor"},
	"IssueComment":                     {"id", "body", "author", "createdAt", "updatedAt", "url"},
	"CreateIssuePayload":               {"issue", "clientMutationId"},
	"CloseIssuePayload":                {"issue", "clientMutationId"},
	"CreatePullRequestPayload":         {"pullRequest", "clientMutationId"},
	"MergePullRequestPayload":          {"pullRequest", "clientMutationId"},
	"AddCommentPayload":                {"commentEdge", "subject", "clientMutationId"},
	"CommentEdge":                      {"node", "cursor"},
	"__Type":                           {"name", "kind", "description", "fields", "enumValues", "possibleTypes", "inputFields", "specifiedByURL"},
	"__Field":                          {"name", "description", "args", "type", "isDeprecated", "deprecationReason"},
	"__EnumValue":                      {"name", "description", "isDeprecated", "deprecationReason"},
	"__Schema":                         {"types", "queryType", "mutationType", "directives"},
}

var enumValues = map[string][]string{
	"SearchType":             {"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER", "ORGANIZATION", "MARKETPLACE", "DISCUSSION", "CODE", "COMMIT", "TOPIC", "WIKI"},
	"IssueState":             {"OPEN", "CLOSED"},
	"PullRequestState":       {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":         {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"RepositoryPermission":   {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"IssueOrderField":        {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"OrderDirection":         {"ASC", "DESC"},
	"PullRequestMergeMethod": {"MERGE", "SQUASH", "REBASE"},
	"MergeStateStatus":       {"CLEAN", "DIRTY", "BLOCKED", "BEHIND", "UNSTABLE", "UNKNOWN"},
}

func typeKind(name string) string {
	if _, ok := objectFields[name]; ok {
		return "OBJECT"
	}
	if _, ok := enumValues[name]; ok {
		return "ENUM"
	}
	if name == "Node" {
		return "INTERFACE"
	}
	switch name {
	case "String", "Int", "Boolean", "ID", "Float":
		return "SCALAR"
	}
	return ""
}

// ---------------------------------------------------------------------------
// Executor

// A resolved object: T is its GraphQL type; the rest is resolver data.
type gqlSource struct {
	T         string
	Repo      *repoRec
	Issue     *issueRec
	Pull      *pullRec
	Login     string
	Str       string // Ref name, __Type name, __Field name, __EnumValue name
	AllIssues []*issueRec
	AllPulls  []*pullRec
	Changes   []fileChange
	First     int
	After     *string
	HasNext   bool
	EndCursor *string
	IssueDesc *gqlSource
	PullDesc  *gqlSource
	Comment   *commentRec
}

type execContext struct {
	variables map[string]interface{}
	fragments map[string]*fragmentNode
	errors    []map[string]interface{}
}

type fieldError struct{ message string }

func (e *fieldError) Error() string { return e.message }

func repoSource(repo *repoRec) *gqlSource {
	return &gqlSource{T: "Repository", Repo: repo}
}

func issueSource(repo *repoRec, issue *issueRec) *gqlSource {
	return &gqlSource{T: "Issue", Repo: repo, Issue: issue}
}

func pullSource(repo *repoRec, pull *pullRec) *gqlSource {
	return &gqlSource{T: "PullRequest", Repo: repo, Pull: pull}
}

func cursorFor(number int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("issue-cursor:%d", number)))
}

func argString(args map[string]interface{}, key string) string {
	value, ok := args[key]
	if !ok {
		return ""
	}
	return jsString(value)
}

func argNumber(args map[string]interface{}, key string) (int, bool) {
	value, ok := args[key]
	if !ok {
		return 0, false
	}
	number, isNumber := jsNumber(value)
	if !isNumber || number != float64(int(number)) {
		return 0, false
	}
	return int(number), true
}

func resolveField(source *gqlSource, field *fieldNode, ctx *execContext) (interface{}, error) {
	typeName := source.T
	args := map[string]interface{}{}
	for _, key := range field.argOrder {
		args[key] = valueOf(field.args[key], ctx.variables)
	}
	repo := source.Repo

	switch typeName {
	case "Query":
		switch field.name {
		case "viewer":
			return &gqlSource{T: "User", Login: viewer.Login}, nil
		case "repository":
			found := getRepo(argString(args, "owner"), argString(args, "name"))
			if found == nil {
				return nil, nil
			}
			return repoSource(found), nil
		case "node":
			decodedType, decodedID, ok := decodeGlobalID(argString(args, "id"))
			if !ok {
				return nil, nil
			}
			switch decodedType {
			case "Repository":
				found := findRepoByID(decodedID)
				if found == nil {
					return nil, nil
				}
				return repoSource(found), nil
			case "Issue":
				found := findIssueByID(decodedID)
				if found == nil {
					return nil, nil
				}
				return issueSource(found.Repo, found.Issue), nil
			case "PullRequest":
				found := findPullByID(decodedID)
				if found == nil {
					return nil, nil
				}
				return pullSource(found.Repo, found.Pull), nil
			}
			return nil, nil
		case "__type":
			name := argString(args, "name")
			if typeKind(name) == "" {
				return nil, nil
			}
			return &gqlSource{T: "__Type", Str: name}, nil
		case "__schema":
			return &gqlSource{T: "__Schema"}, nil
		}
		return nil, nil

	case "Mutation":
		input, _ := args["input"].(map[string]interface{})
		if input == nil {
			input = map[string]interface{}{}
		}
		switch field.name {
		case "createIssue":
			decodedType, decodedID, ok := decodeGlobalID(jsString(input["repositoryId"]))
			var target *repoRec
			if ok && decodedType == "Repository" {
				target = findRepoByID(decodedID)
			}
			if target == nil {
				return nil, &fieldError{"repository not found for createIssue"}
			}
			body, _ := input["body"].(string)
			issue := createIssue(target, jsString(input["title"]), body, nil)
			return &gqlSource{T: "CreateIssuePayload", IssueDesc: issueSource(target, issue)}, nil
		case "closeIssue":
			decodedType, decodedID, ok := decodeGlobalID(jsString(input["issueId"]))
			var found *issueLocation
			if ok && decodedType == "Issue" {
				found = findIssueByID(decodedID)
			}
			if found == nil {
				return nil, &fieldError{"issue not found for closeIssue"}
			}
			found.Issue.State = "closed"
			return &gqlSource{T: "CloseIssuePayload", IssueDesc: issueSource(found.Repo, found.Issue)}, nil
		case "createPullRequest":
			decodedType, decodedID, ok := decodeGlobalID(jsString(input["repositoryId"]))
			var target *repoRec
			if ok && decodedType == "Repository" {
				target = findRepoByID(decodedID)
			}
			if target == nil {
				return nil, &fieldError{"repository not found for createPullRequest"}
			}
			head := jsString(input["headRefName"])
			base := jsString(input["baseRefName"])
			headSha, hasHead := target.ref("refs/heads/" + head)
			baseSha, hasBase := target.ref("refs/heads/" + base)
			if !hasHead || !hasBase {
				return nil, &fieldError{"head or base branch does not exist"}
			}
			body, _ := input["body"].(string)
			pull := createPull(target, jsString(input["title"]), body, head, base, headSha, baseSha)
			return &gqlSource{T: "CreatePullRequestPayload", PullDesc: pullSource(target, pull)}, nil
		case "mergePullRequest":
			decodedType, decodedID, ok := decodeGlobalID(jsString(input["pullRequestId"]))
			var found *pullLocation
			if ok && decodedType == "PullRequest" {
				found = findPullByID(decodedID)
			}
			if found == nil {
				return nil, &fieldError{"pull request not found for mergePullRequest"}
			}
			if found.Pull.State == "open" {
				outcome := squashMerge(found.Repo, found.Pull)
				if !outcome.OK {
					return nil, &fieldError{"Pull Request is not mergeable"}
				}
			}
			return &gqlSource{T: "MergePullRequestPayload", PullDesc: pullSource(found.Repo, found.Pull)}, nil
		case "addComment":
			decodedType, decodedID, ok := decodeGlobalID(jsString(input["subjectId"]))
			var found *issueLocation
			if ok && decodedType == "Issue" {
				found = findIssueByID(decodedID)
			}
			if found == nil {
				return nil, &fieldError{"subject not found for addComment"}
			}
			comment := addComment(found.Issue, jsString(input["body"]))
			return &gqlSource{T: "AddCommentPayload", Comment: comment, IssueDesc: issueSource(found.Repo, found.Issue)}, nil
		}
		return nil, nil

	case "User":
		switch field.name {
		case "login":
			return source.Login, nil
		case "id":
			return encodeGlobalID("User", viewer.ID), nil
		case "name":
			return viewer.Name, nil
		case "email":
			return viewer.Email, nil
		}
		return nil, nil

	case "Repository":
		record := repo
		switch field.name {
		case "id":
			return encodeGlobalID("Repository", record.ID), nil
		case "databaseId":
			return record.ID, nil
		case "name":
			return record.Name, nil
		case "nameWithOwner":
			return record.Owner + "/" + record.Name, nil
		case "owner":
			return &gqlSource{T: "User", Login: record.Owner}, nil
		case "defaultBranchRef":
			if _, ok := record.ref("refs/heads/" + record.DefaultBranch); ok {
				return &gqlSource{T: "Ref", Str: record.DefaultBranch}, nil
			}
			return nil, nil
		case "viewerPermission":
			return "ADMIN", nil
		case "hasIssuesEnabled":
			return true, nil
		case "description":
			return record.Description, nil
		case "isPrivate":
			return record.Priv, nil
		case "issue":
			number, ok := argNumber(args, "number")
			if !ok {
				return nil, nil
			}
			issue := record.issues[number]
			if issue == nil {
				return nil, nil
			}
			return issueSource(record, issue), nil
		case "issues":
			list := []*issueRec{}
			for _, number := range record.issueOrder {
				list = append(list, record.issues[number])
			}
			if states, ok := args["states"].([]interface{}); ok {
				wanted := map[string]bool{}
				for _, state := range states {
					wanted[strings.ToLower(jsString(state))] = true
				}
				filtered := []*issueRec{}
				for _, issue := range list {
					if wanted[issue.State] {
						filtered = append(filtered, issue)
					}
				}
				list = filtered
			}
			direction := ""
			if orderBy, ok := args["orderBy"].(map[string]interface{}); ok {
				direction, _ = orderBy["direction"].(string)
			}
			sort.SliceStable(list, func(i, j int) bool {
				if direction == "DESC" {
					return list[i].Number > list[j].Number
				}
				return list[i].Number < list[j].Number
			})
			first := 30
			if value, ok := args["first"].(float64); ok {
				first = int(value)
			}
			var after *string
			if value, ok := args["after"].(string); ok {
				after = &value
			}
			return &gqlSource{T: "IssueConnection", Repo: record, AllIssues: list, First: first, After: after}, nil
		case "pullRequest":
			number, ok := argNumber(args, "number")
			if !ok {
				return nil, nil
			}
			pull := record.pulls[number]
			if pull == nil {
				return nil, nil
			}
			return pullSource(record, pull), nil
		case "pullRequests":
			open := []*pullRec{}
			for _, number := range record.pullOrder {
				pull := record.pulls[number]
				if pull.State == "open" {
					open = append(open, pull)
				}
			}
			sort.SliceStable(open, func(i, j int) bool { return open[i].Number < open[j].Number })
			first := 30
			if value, ok := args["first"].(float64); ok {
				first = int(value)
			}
			return &gqlSource{T: "PullRequestConnection", Repo: record, AllPulls: open, First: first}, nil
		}
		return nil, nil

	case "Ref":
		if field.name == "name" {
			return source.Str, nil
		}
		return nil, nil

	case "Issue":
		issue := source.Issue
		switch field.name {
		case "id":
			return encodeGlobalID("Issue", issue.ID), nil
		case "databaseId":
			return issue.ID, nil
		case "number":
			return issue.Number, nil
		case "title":
			return issue.Title, nil
		case "body":
			return issue.Body, nil
		case "state":
			return strings.ToUpper(issue.State), nil
		}
		return nil, nil

	case "IssueConnection":
		all := source.AllIssues
		first := source.First
		start := 0
		if source.After != nil {
			decoded := lenientBase64ToString(*source.After)
			if idx := strings.Index(decoded, "issue-cursor:"); idx >= 0 {
				decoded = decoded[:idx] + decoded[idx+len("issue-cursor:"):]
			}
			if number, ok := jsParseInt(decoded); ok {
				for index, issue := range all {
					if int64(issue.Number) == number {
						start = index + 1
						break
					}
				}
			}
		}
		end := start + first
		if end > len(all) {
			end = len(all)
		}
		if end < start {
			end = start
		}
		slice := all[start:end]
		switch field.name {
		case "nodes":
			out := make([]*gqlSource, 0, len(slice))
			for _, issue := range slice {
				out = append(out, issueSource(repo, issue))
			}
			return out, nil
		case "edges":
			out := make([]*gqlSource, 0, len(slice))
			for _, issue := range slice {
				cursor := cursorFor(issue.Number)
				out = append(out, &gqlSource{T: "IssueEdge", Repo: repo, Issue: issue, EndCursor: &cursor})
			}
			return out, nil
		case "pageInfo":
			var endCursor *string
			if len(slice) > 0 {
				cursor := cursorFor(slice[len(slice)-1].Number)
				endCursor = &cursor
			}
			return &gqlSource{T: "PageInfo", HasNext: start+len(slice) < len(all), EndCursor: endCursor}, nil
		case "totalCount":
			return len(all), nil
		}
		return nil, nil

	case "IssueEdge":
		switch field.name {
		case "node":
			return issueSource(repo, source.Issue), nil
		case "cursor":
			return *source.EndCursor, nil
		}
		return nil, nil

	case "PullRequest":
		pull := source.Pull
		record := repo
		switch field.name {
		case "id":
			return encodeGlobalID("PullRequest", pull.ID), nil
		case "databaseId":
			return pull.ID, nil
		case "number":
			return pull.Number, nil
		case "title":
			return pull.Title, nil
		case "body":
			return pull.Body, nil
		case "state":
			if pull.Merged {
				return "MERGED", nil
			}
			return strings.ToUpper(pull.State), nil
		case "merged":
			return pull.Merged, nil
		case "mergeable":
			return mergeableState(record, pull), nil
		case "headRefName":
			return pull.HeadRef, nil
		case "baseRefName":
			return pull.BaseRef, nil
		case "files":
			first := 30
			if value, ok := args["first"].(float64); ok {
				first = int(value)
			}
			return &gqlSource{T: "PullRequestChangedFileConnection", Changes: pullChanges(record, pull), First: first}, nil
		}
		return nil, nil

	case "PullRequestConnection":
		all := source.AllPulls
		first := source.First
		end := first
		if end > len(all) {
			end = len(all)
		}
		if end < 0 {
			end = 0
		}
		slice := all[:end]
		switch field.name {
		case "nodes":
			out := make([]*gqlSource, 0, len(slice))
			for _, pull := range slice {
				out = append(out, pullSource(repo, pull))
			}
			return out, nil
		case "pageInfo":
			return &gqlSource{T: "PageInfo", HasNext: len(all) > len(slice), EndCursor: nil}, nil
		case "totalCount":
			return len(all), nil
		}
		return nil, nil

	case "PullRequestChangedFileConnection":
		changes := source.Changes
		first := source.First
		end := first
		if end > len(changes) {
			end = len(changes)
		}
		if end < 0 {
			end = 0
		}
		slice := changes[:end]
		switch field.name {
		case "nodes":
			out := make([]*gqlSource, 0, len(slice))
			for _, change := range slice {
				changeCopy := change
				out = append(out, &gqlSource{T: "PullRequestChangedFile", Changes: []fileChange{changeCopy}})
			}
			return out, nil
		case "totalCount":
			return len(changes), nil
		case "pageInfo":
			return &gqlSource{T: "PageInfo", HasNext: len(changes) > len(slice), EndCursor: nil}, nil
		}
		return nil, nil

	case "PullRequestChangedFile":
		change := source.Changes[0]
		switch field.name {
		case "path":
			return change.Path, nil
		case "additions":
			return change.Additions, nil
		case "deletions":
			return change.Deletions, nil
		case "changeType":
			if change.Status == "removed" {
				return "DELETED", nil
			}
			return strings.ToUpper(change.Status), nil
		}
		return nil, nil

	case "PageInfo":
		switch field.name {
		case "hasNextPage":
			return source.HasNext, nil
		case "endCursor":
			if source.EndCursor == nil {
				return nil, nil
			}
			return *source.EndCursor, nil
		case "hasPreviousPage":
			return false, nil
		case "startCursor":
			if source.EndCursor == nil {
				return nil, nil
			}
			return *source.EndCursor, nil
		}
		return nil, nil

	case "CreateIssuePayload", "CloseIssuePayload":
		if field.name == "issue" {
			return source.IssueDesc, nil
		}
		return nil, nil

	case "CreatePullRequestPayload", "MergePullRequestPayload":
		if field.name == "pullRequest" {
			return source.PullDesc, nil
		}
		return nil, nil

	case "AddCommentPayload":
		switch field.name {
		case "commentEdge":
			return &gqlSource{T: "CommentEdge", Comment: source.Comment}, nil
		case "subject":
			return source.IssueDesc, nil
		}
		return nil, nil

	case "CommentEdge":
		switch field.name {
		case "node":
			return &gqlSource{T: "IssueComment", Comment: source.Comment}, nil
		case "cursor":
			return cursorFor(source.Comment.ID), nil
		}
		return nil, nil

	case "IssueComment":
		switch field.name {
		case "id":
			return encodeGlobalID("IssueComment", source.Comment.ID), nil
		case "body":
			return source.Comment.Body, nil
		}
		return nil, nil

	case "__Type":
		name := source.Str
		switch field.name {
		case "name":
			return name, nil
		case "kind":
			kind := typeKind(name)
			if kind == "" {
				return nil, nil
			}
			return kind, nil
		case "fields":
			out := []*gqlSource{}
			for _, fieldName := range objectFields[name] {
				out = append(out, &gqlSource{T: "__Field", Str: fieldName})
			}
			return out, nil
		case "enumValues":
			out := []*gqlSource{}
			for _, valueName := range enumValues[name] {
				out = append(out, &gqlSource{T: "__EnumValue", Str: valueName})
			}
			return out, nil
		}
		return nil, nil

	case "__Field":
		if field.name == "name" {
			return source.Str, nil
		}
		return nil, nil

	case "__EnumValue":
		if field.name == "name" {
			return source.Str, nil
		}
		return nil, nil
	}
	return nil, nil
}

func executeSelectionSet(source *gqlSource, selections []selectionNode, ctx *execContext) (map[string]interface{}, error) {
	out := map[string]interface{}{}
	for _, selection := range selections {
		if selection.kind == selectionInline {
			if selection.hasCondition && selection.typeCondition != "" && selection.typeCondition != source.T && selection.typeCondition != "Node" {
				continue
			}
			merged, err := executeSelectionSet(source, selection.selectionSet, ctx)
			if err != nil {
				return nil, err
			}
			for key, value := range merged {
				out[key] = value
			}
			continue
		}
		if selection.kind == selectionSpread {
			fragment := ctx.fragments[selection.spreadName]
			if fragment == nil {
				continue
			}
			if fragment.typeCondition != "" && fragment.typeCondition != source.T && fragment.typeCondition != "Node" {
				continue
			}
			merged, err := executeSelectionSet(source, fragment.selectionSet, ctx)
			if err != nil {
				return nil, err
			}
			for key, value := range merged {
				out[key] = value
			}
			continue
		}
		field := selection.field
		if field.name == "__typename" {
			key := field.name
			if field.hasAlias {
				key = field.alias
			}
			out[key] = source.T
			continue
		}
		if fields, ok := objectFields[source.T]; ok {
			found := false
			for _, name := range fields {
				if name == field.name {
					found = true
					break
				}
			}
			if !found {
				return nil, &fieldError{fmt.Sprintf("Field '%s' doesn't exist on type '%s'", field.name, source.T)}
			}
		}
		value, err := resolveField(source, field, ctx)
		if err != nil {
			return nil, err
		}
		key := field.name
		if field.hasAlias {
			key = field.alias
		}
		if value == nil {
			out[key] = nil
			continue
		}
		if list, ok := value.([]*gqlSource); ok {
			items := []interface{}{}
			for _, item := range list {
				if field.hasSelection {
					sub, err := executeSelectionSet(item, field.selectionSet, ctx)
					if err != nil {
						return nil, err
					}
					items = append(items, sub)
				} else {
					items = append(items, nil)
				}
			}
			out[key] = items
			continue
		}
		if child, ok := value.(*gqlSource); ok {
			if field.hasSelection {
				sub, err := executeSelectionSet(child, field.selectionSet, ctx)
				if err != nil {
					return nil, err
				}
				out[key] = sub
			} else {
				out[key] = nil
			}
			continue
		}
		out[key] = value
	}
	return out, nil
}

func executeGraphQLOperation(document *documentNode, operationName string, hasOperationName bool, variables map[string]interface{}) interface{} {
	var operation *operationNode
	if hasOperationName {
		for i := range document.operations {
			if document.operations[i].hasName && document.operations[i].name == operationName {
				operation = &document.operations[i]
				break
			}
		}
		if operation == nil && len(document.operations) > 0 {
			operation = &document.operations[0]
		}
	} else if len(document.operations) > 0 {
		operation = &document.operations[0]
	}
	if operation == nil {
		return map[string]interface{}{"errors": []interface{}{map[string]interface{}{"message": "no operation"}}}
	}
	ctx := &execContext{variables: variables, fragments: document.fragments}
	root := &gqlSource{T: "Query"}
	if operation.opType == "mutation" {
		root = &gqlSource{T: "Mutation"}
	}
	data, err := executeSelectionSet(root, operation.selectionSet, ctx)
	if err != nil {
		return map[string]interface{}{
			"data":   nil,
			"errors": []interface{}{map[string]interface{}{"message": err.Error()}},
		}
	}
	return map[string]interface{}{"data": data}
}

func executeGraphQLBody(body map[string]interface{}) interface{} {
	queryText, _ := body["query"].(string)
	variables, _ := body["variables"].(map[string]interface{})
	if variables == nil {
		variables = map[string]interface{}{}
	}
	operationName, hasOperationName := body["operationName"].(string)
	parser := &gqlParser{tokens: tokenizeGraphQL(queryText)}
	document, err := parser.parseDocument()
	if err != nil {
		return map[string]interface{}{"errors": []interface{}{map[string]interface{}{"message": err.Error()}}}
	}
	return executeGraphQLOperation(document, operationName, hasOperationName, variables)
}

// handleGraphQL serves POST /api/graphql with the already-parsed body:
// a single operation object, or a batch array of operations (each
// answered with its own result body). A non-object body is a 400.
func handleGraphQL(parsed interface{}) (int, interface{}) {
	if batch, ok := parsed.([]interface{}); ok {
		out := make([]interface{}, 0, len(batch))
		for _, item := range batch {
			operation, _ := item.(map[string]interface{})
			if operation == nil {
				operation = map[string]interface{}{}
			}
			out = append(out, executeGraphQLBody(operation))
		}
		return 200, out
	}
	if single, ok := parsed.(map[string]interface{}); ok {
		return 200, executeGraphQLBody(single)
	}
	return 400, map[string]interface{}{"errors": []interface{}{map[string]interface{}{"message": "a GraphQL request body is required"}}}
}
