package main

// The GitHub REST v3 surface: repositories, branches, commits, contents,
// labels, issues, comments and pull requests, with GitHub's statuses and
// JSON shapes.

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

type restRequest struct {
	Method string
	// Segments are the decoded path segments after /api/v3.
	Segments []string
	Query    url.Values
	// Body is the parsed JSON object body, or nil when there was no body.
	Body map[string]interface{}
	// BodyError is true when a request body was present but not valid JSON.
	BodyError bool
	// Host is the Host header, for absolute pagination URLs.
	Host string
}

type restReply struct {
	Status  int
	Body    interface{} // nil means no body
	Headers map[string]string
}

var restNotFound = restReply{Status: 404, Body: map[string]interface{}{"message": "Not Found"}}

func restMessage(status int, text string) restReply {
	return restReply{Status: status, Body: map[string]interface{}{"message": text}}
}

func (req *restRequest) queryGet(key string) (string, bool) {
	values, ok := req.Query[key]
	if !ok || len(values) == 0 {
		return "", false
	}
	return values[0], true
}

func bodyString(body map[string]interface{}, key string) (string, bool) {
	if body == nil {
		return "", false
	}
	value, ok := body[key].(string)
	return value, ok
}

func bodyBool(body map[string]interface{}, key string) (bool, bool) {
	if body == nil {
		return false, false
	}
	value, ok := body[key].(bool)
	return value, ok
}

func repoJSON(repo *repoRec) map[string]interface{} {
	return map[string]interface{}{
		"id":             repo.ID,
		"name":           repo.Name,
		"full_name":      repo.Owner + "/" + repo.Name,
		"owner":          map[string]interface{}{"login": repo.Owner, "id": viewer.ID, "type": "User"},
		"private":        repo.Priv,
		"description":    repo.Description,
		"default_branch": repo.DefaultBranch,
	}
}

func labelJSON(name, color string) map[string]interface{} {
	return map[string]interface{}{"name": name, "color": color, "description": nil, "default": false}
}

func issueJSON(repo *repoRec, issue *issueRec) map[string]interface{} {
	labels := make([]interface{}, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		color := "ededed"
		if label, ok := repo.labels[name]; ok {
			color = label.Color
		}
		labels = append(labels, labelJSON(name, color))
	}
	return map[string]interface{}{
		"id":       issue.ID,
		"number":   issue.Number,
		"title":    issue.Title,
		"body":     issue.Body,
		"state":    issue.State,
		"labels":   labels,
		"user":     map[string]interface{}{"login": repo.Owner, "id": viewer.ID, "type": "User"},
		"comments": len(issue.Comments),
	}
}

func commentJSON(body string, id int, owner string) map[string]interface{} {
	return map[string]interface{}{
		"id":   id,
		"body": body,
		"user": map[string]interface{}{"login": owner, "id": viewer.ID, "type": "User"},
	}
}

func pullJSON(repo *repoRec, pull *pullRec) map[string]interface{} {
	headSha, ok := repo.ref("refs/heads/" + pull.HeadRef)
	if !ok {
		headSha = pull.CreatedHeadSha
	}
	baseSha, ok := repo.ref("refs/heads/" + pull.BaseRef)
	if !ok {
		baseSha = pull.CreatedBaseSha
	}
	var mergedAt interface{}
	if pull.MergedAt != nil {
		mergedAt = *pull.MergedAt
	}
	var mergeCommitSha interface{}
	if pull.MergeCommitSha != nil {
		mergeCommitSha = *pull.MergeCommitSha
	}
	return map[string]interface{}{
		"id":               pull.ID,
		"number":           pull.Number,
		"state":            pull.State,
		"title":            pull.Title,
		"body":             pull.Body,
		"head":             map[string]interface{}{"ref": pull.HeadRef, "sha": headSha, "label": repo.Owner + ":" + pull.HeadRef},
		"base":             map[string]interface{}{"ref": pull.BaseRef, "sha": baseSha, "label": repo.Owner + ":" + pull.BaseRef},
		"merged":           pull.Merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeCommitSha,
		"user":             map[string]interface{}{"login": repo.Owner, "id": viewer.ID, "type": "User"},
	}
}

func parsePage(query url.Values) (int, int) {
	perPage := 30
	if raw, present := query["per_page"]; present && len(raw) > 0 {
		if parsed, ok := jsParseInt(raw[0]); ok && parsed > 0 {
			perPage = int(parsed)
			if perPage > 100 {
				perPage = 100
			}
		}
	}
	page := 1
	if raw, present := query["page"]; present && len(raw) > 0 {
		if parsed, ok := jsParseInt(raw[0]); ok && parsed > 0 {
			page = int(parsed)
		}
	}
	return page, perPage
}

func paginate(items []interface{}, query url.Values, basePath, host string) ([]interface{}, map[string]string) {
	page, perPage := parsePage(query)
	start := (page - 1) * perPage
	slice := []interface{}{}
	if start < len(items) {
		end := start + perPage
		if end > len(items) {
			end = len(items)
		}
		slice = items[start:end]
	}
	headers := map[string]string{}
	lastPage := (len(items) + perPage - 1) / perPage
	if lastPage < 1 {
		lastPage = 1
	}
	if page < lastPage {
		urlFor := func(p int) string {
			return fmt.Sprintf("http://%s%s?per_page=%d&page=%d", host, basePath, perPage, p)
		}
		parts := []string{
			fmt.Sprintf("<%s>; rel=\"next\"", urlFor(page+1)),
			fmt.Sprintf("<%s>; rel=\"last\"", urlFor(lastPage)),
		}
		if page > 1 {
			parts = append(parts,
				fmt.Sprintf("<%s>; rel=\"first\"", urlFor(1)),
				fmt.Sprintf("<%s>; rel=\"prev\"", urlFor(page-1)))
		}
		headers["link"] = strings.Join(parts, ", ")
	}
	return slice, headers
}

// ---------------------------------------------------------------------------

func handleRest(req *restRequest) restReply {
	seg := req.Segments
	method := req.Method

	if req.BodyError && (method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE") {
		return restMessage(400, "Problems parsing JSON")
	}

	// API root and meta.
	if len(seg) == 0 {
		if method != "GET" {
			return restNotFound
		}
		return restReply{
			Status: 200,
			Body: map[string]interface{}{
				"current_user_url": "/api/v3/user",
				"repository_url":   "/api/v3/repos/{owner}/{repo}",
				"issue_search_url": "/api/v3/search/issues",
				"rate_limit_url":   "/api/v3/rate_limit",
			},
			Headers: map[string]string{"x-oauth-scopes": "repo, read:org"},
		}
	}
	if len(seg) == 1 && seg[0] == "meta" {
		if method != "GET" {
			return restNotFound
		}
		return restReply{Status: 200, Body: map[string]interface{}{"installed_version": "1.0.0", "verifiable_password_authentication": false}}
	}
	if len(seg) == 1 && seg[0] == "user" {
		if method != "GET" {
			return restNotFound
		}
		return restReply{Status: 200, Body: map[string]interface{}{
			"login": viewer.Login, "id": viewer.ID, "name": viewer.Name, "email": viewer.Email, "type": "User",
		}}
	}

	// The viewer's repositories.
	if len(seg) == 2 && seg[0] == "user" && seg[1] == "repos" {
		if method == "POST" {
			name, ok := bodyString(req.Body, "name")
			if !ok || name == "" {
				return restMessage(422, "Validation Failed: name is required")
			}
			if getRepo(viewer.Login, name) != nil {
				return restMessage(422, "Repository already exists")
			}
			description, _ := bodyString(req.Body, "description")
			priv, _ := bodyBool(req.Body, "private")
			repo := createRepo(viewer.Login, name, description, priv)
			return restReply{Status: 201, Body: repoJSON(repo)}
		}
		if method == "GET" {
			all := []interface{}{}
			for _, repo := range viewerRepos() {
				all = append(all, repoJSON(repo))
			}
			page, headers := paginate(all, req.Query, "/api/v3/user/repos", req.Host)
			return restReply{Status: 200, Body: page, Headers: headers}
		}
		return restNotFound
	}

	if len(seg) >= 3 && seg[0] == "repos" {
		repo := getRepo(seg[1], seg[2])
		if repo == nil {
			return restNotFound
		}
		return handleRepoRest(req, repo, seg[3:])
	}

	return restNotFound
}

func handleRepoRest(req *restRequest, repo *repoRec, seg []string) restReply {
	method := req.Method

	if len(seg) == 0 {
		if method == "GET" {
			return restReply{Status: 200, Body: repoJSON(repo)}
		}
		if method == "PATCH" {
			if description, ok := bodyString(req.Body, "description"); ok {
				repo.Description = description
			}
			if priv, ok := bodyBool(req.Body, "private"); ok {
				repo.Priv = priv
			}
			if branch, ok := bodyString(req.Body, "default_branch"); ok && branch != "" {
				repo.DefaultBranch = branch
			}
			return restReply{Status: 200, Body: repoJSON(repo)}
		}
		if method == "DELETE" {
			deleteRepo(repo.Owner, repo.Name)
			return restReply{Status: 204}
		}
		return restNotFound
	}

	if seg[0] == "branches" {
		if len(seg) == 1 && method == "GET" {
			branches := []interface{}{}
			for _, ref := range repo.refOrder {
				if !strings.HasPrefix(ref, "refs/heads/") {
					continue
				}
				branches = append(branches, map[string]interface{}{
					"name":      ref[len("refs/heads/"):],
					"commit":    map[string]interface{}{"sha": repo.refs[ref]},
					"protected": false,
				})
			}
			return restReply{Status: 200, Body: branches}
		}
		if len(seg) >= 2 && method == "GET" {
			branch := strings.Join(seg[1:], "/")
			sha, ok := repo.ref("refs/heads/" + branch)
			if !ok {
				return restNotFound
			}
			return restReply{Status: 200, Body: map[string]interface{}{
				"name": branch, "commit": map[string]interface{}{"sha": sha}, "protected": false,
			}}
		}
		return restNotFound
	}

	if seg[0] == "commits" && len(seg) == 1 && method == "GET" {
		wanted, present := req.queryGet("sha")
		if !present {
			wanted = repo.DefaultBranch
		}
		tip := resolveRef(repo, wanted)
		if tip == "" {
			if len(repo.refs) == 0 {
				return restReply{Status: 200, Body: []interface{}{}}
			}
			return restNotFound
		}
		// ancestorDistances returns oids in BFS order, which is exactly
		// the stable sort by distance the REST shape expects.
		_, order := ancestorDistances(tip)
		ordered := make([]interface{}, 0, len(order))
		for _, oid := range order {
			ordered = append(ordered, oid)
		}
		page, headers := paginate(ordered, req.Query, "/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/commits", req.Host)
		body := make([]interface{}, 0, len(page))
		for _, item := range page {
			oid := item.(string)
			commit := parseCommit(objects[oid].Data)
			parents := make([]interface{}, 0, len(commit.Parents))
			for _, parent := range commit.Parents {
				parents = append(parents, map[string]interface{}{"sha": parent})
			}
			body = append(body, map[string]interface{}{
				"sha": oid,
				"commit": map[string]interface{}{
					"message":   commit.Message,
					"author":    identityJSON(commit.Author),
					"committer": identityJSON(commit.Committer),
				},
				"parents": parents,
			})
		}
		return restReply{Status: 200, Body: body, Headers: headers}
	}

	if seg[0] == "git" && len(seg) >= 2 && seg[1] == "refs" {
		if len(seg) == 2 && method == "POST" {
			ref, _ := bodyString(req.Body, "ref")
			sha, _ := bodyString(req.Body, "sha")
			if ref == "" || !strings.HasPrefix(ref, "refs/") {
				return restMessage(422, "Validation Failed: ref is required")
			}
			object, exists := objects[sha]
			if !exists {
				return restMessage(422, "Validation Failed: object does not exist")
			}
			if _, exists := repo.ref(ref); exists {
				return restMessage(422, "Reference already exists")
			}
			repo.setRef(ref, sha)
			return restReply{Status: 201, Body: map[string]interface{}{
				"ref": ref, "object": map[string]interface{}{"sha": sha, "type": object.Type},
			}}
		}
		if len(seg) >= 3 && method == "GET" {
			tail := strings.Join(seg[2:], "/")
			full := tail
			if !strings.HasPrefix(tail, "refs/") {
				full = "refs/" + tail
			}
			sha, ok := repo.ref(full)
			if !ok {
				return restNotFound
			}
			objType := "commit"
			if object, exists := objects[sha]; exists {
				objType = object.Type
			}
			return restReply{Status: 200, Body: map[string]interface{}{
				"ref": full, "object": map[string]interface{}{"sha": sha, "type": objType},
			}}
		}
		return restNotFound
	}

	if seg[0] == "contents" {
		return handleContents(req, repo, strings.Join(seg[1:], "/"))
	}

	if seg[0] == "labels" {
		if len(seg) == 1 && method == "POST" {
			name, ok := bodyString(req.Body, "name")
			if !ok || name == "" {
				return restMessage(422, "Validation Failed: name is required")
			}
			if _, exists := repo.labels[name]; exists {
				return restMessage(422, "Validation Failed: label already exists")
			}
			color, isString := bodyString(req.Body, "color")
			if !isString {
				color = "ededed"
			}
			repo.labels[name] = &labelRec{Name: name, Color: color}
			repo.labelOrder = append(repo.labelOrder, name)
			return restReply{Status: 201, Body: labelJSON(name, color)}
		}
		if len(seg) == 1 && method == "GET" {
			out := []interface{}{}
			for _, name := range repo.labelOrder {
				label := repo.labels[name]
				out = append(out, labelJSON(label.Name, label.Color))
			}
			return restReply{Status: 200, Body: out}
		}
		return restNotFound
	}

	if seg[0] == "issues" {
		return handleIssues(req, repo, seg[1:])
	}

	if seg[0] == "pulls" {
		return handlePulls(req, repo, seg[1:])
	}

	return restNotFound
}

var identityPattern = regexp.MustCompile(`^(.*?) <(.*?)> (\d+)`)

func identityJSON(line string) map[string]interface{} {
	match := identityPattern.FindStringSubmatch(line)
	name, email, stamp := "", "", int64(0)
	if match != nil {
		name = match[1]
		email = match[2]
		stamp, _ = jsParseInt(match[3])
	}
	return map[string]interface{}{"name": name, "email": email, "date": isoFromUnixSeconds(stamp)}
}

// ---------------------------------------------------------------------------

// targetBranchTip resolves the branch a contents write lands on: an
// existing branch, or a new branch forked from the default branch's tip.
func targetBranchTip(repo *repoRec, branch *string) (string, string, bool) {
	name := repo.DefaultBranch
	if branch != nil {
		name = *branch
	}
	if existing, ok := repo.ref("refs/heads/" + name); ok {
		return name, existing, true
	}
	// A contents write naming a new branch forks it from the default
	// branch's tip.
	if name != repo.DefaultBranch {
		if baseTip, ok := repo.ref("refs/heads/" + repo.DefaultBranch); ok {
			repo.setRef("refs/heads/"+name, baseTip)
			return name, baseTip, true
		}
	}
	return name, "", false
}

var base64Whitespace = regexp.MustCompile(`[\r\n\t ]+`)

func handleContents(req *restRequest, repo *repoRec, path string) restReply {
	method := req.Method

	if method == "GET" {
		ref, present := req.queryGet("ref")
		if !present {
			ref = repo.DefaultBranch
		}
		commit := resolveRef(repo, ref)
		if commit == "" {
			return restNotFound
		}
		treeOid := treeOfCommit(commit)
		if path == "" {
			return restReply{Status: 200, Body: listTree(treeOid, "")}
		}
		segments := strings.Split(path, "/")
		for i, segment := range segments {
			object, ok := objects[treeOid]
			if !ok || object.Type != "tree" {
				return restNotFound
			}
			var found *treeEntry
			for _, entry := range parseTree(object.Data) {
				if entry.Name == segment {
					candidate := entry
					found = &candidate
					break
				}
			}
			if found == nil {
				return restNotFound
			}
			if i == len(segments)-1 {
				if found.Mode == "40000" {
					return restReply{Status: 200, Body: listTree(found.Oid, path)}
				}
				blob, ok := objects[found.Oid]
				if !ok {
					return restNotFound
				}
				return restReply{Status: 200, Body: map[string]interface{}{
					"type":     "file",
					"name":     found.Name,
					"path":     path,
					"sha":      found.Oid,
					"size":     len(blob.Data),
					"encoding": "base64",
					"content":  base64Encode(blob.Data),
				}}
			}
			if found.Mode != "40000" {
				return restNotFound
			}
			treeOid = found.Oid
		}
		return restNotFound
	}

	if method == "PUT" {
		contentB64, ok := bodyString(req.Body, "content")
		if !ok {
			return restMessage(422, "Validation Failed: content is required")
		}
		content := lenientBase64Decode(base64Whitespace.ReplaceAllString(contentB64, ""))
		commitMessage, isString := bodyString(req.Body, "message")
		if !isString {
			commitMessage = "Update file"
		}
		var branchField *string
		if value, isString := bodyString(req.Body, "branch"); isString {
			branchField = &value
		}
		branch, tip, hasTip := targetBranchTip(repo, branchField)
		flat := map[string]string{}
		if hasTip {
			flat = flattenTree(treeOfCommit(tip))
		}
		existing, hasExisting := flat[path]
		shaField, shaIsString := bodyString(req.Body, "sha")
		if hasExisting {
			if !shaIsString {
				return restMessage(422, "Validation Failed: sha is required when updating")
			}
			if shaField != existing {
				return restMessage(409, "Conflict: the sha does not match the current file")
			}
		}
		blobOid := storeObject("blob", content)
		commit := commitChanges(repo, branch, []pathChange{{Path: path, Content: content}}, commitMessage)
		name := path
		if idx := strings.LastIndex(path, "/"); idx >= 0 {
			name = path[idx+1:]
		}
		status := 201
		if hasExisting {
			status = 200
		}
		return restReply{Status: status, Body: map[string]interface{}{
			"content": map[string]interface{}{"name": name, "path": path, "sha": blobOid, "size": len(content), "encoding": "base64"},
			"commit":  map[string]interface{}{"sha": commit},
		}}
	}

	if method == "DELETE" {
		commitMessage, isString := bodyString(req.Body, "message")
		if !isString {
			commitMessage = "Delete file"
		}
		var branchField *string
		if value, isString := bodyString(req.Body, "branch"); isString {
			branchField = &value
		}
		branch, tip, hasTip := targetBranchTip(repo, branchField)
		if !hasTip {
			return restNotFound
		}
		flat := flattenTree(treeOfCommit(tip))
		existing, hasExisting := flat[path]
		if !hasExisting {
			return restNotFound
		}
		if shaField, shaIsString := bodyString(req.Body, "sha"); shaIsString && shaField != existing {
			return restMessage(409, "Conflict: the sha does not match the current file")
		}
		commit := commitChanges(repo, branch, []pathChange{{Path: path, Delete: true}}, commitMessage)
		return restReply{Status: 200, Body: map[string]interface{}{
			"content": nil,
			"commit":  map[string]interface{}{"sha": commit},
		}}
	}

	return restNotFound
}

func base64Encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

func listTree(treeOid, prefix string) []interface{} {
	object, ok := objects[treeOid]
	if !ok || object.Type != "tree" {
		return []interface{}{}
	}
	type row struct {
		name string
		body map[string]interface{}
	}
	rows := []row{}
	for _, entry := range parseTree(object.Data) {
		isDir := entry.Mode == "40000"
		size := 0
		if !isDir {
			if blob, ok := objects[entry.Oid]; ok {
				size = len(blob.Data)
			}
		}
		typ := "file"
		if isDir {
			typ = "dir"
		}
		path := entry.Name
		if prefix != "" {
			path = prefix + "/" + entry.Name
		}
		rows = append(rows, row{name: entry.Name, body: map[string]interface{}{
			"type": typ, "name": entry.Name, "path": path, "sha": entry.Oid, "size": size,
		}})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	out := make([]interface{}, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.body)
	}
	return out
}

// ---------------------------------------------------------------------------

func stringListField(body map[string]interface{}, key string) ([]string, bool) {
	if body == nil {
		return nil, false
	}
	raw, ok := body[key].([]interface{})
	if !ok {
		return nil, false
	}
	out := []string{}
	for _, item := range raw {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out, true
}

func handleIssues(req *restRequest, repo *repoRec, seg []string) restReply {
	method := req.Method

	if len(seg) == 0 {
		if method == "POST" {
			title, ok := bodyString(req.Body, "title")
			if !ok || title == "" {
				return restMessage(422, "Validation Failed: title is required")
			}
			bodyText, _ := bodyString(req.Body, "body")
			labels, _ := stringListField(req.Body, "labels")
			for _, label := range labels {
				if _, exists := repo.labels[label]; !exists {
					return restMessage(422, "Validation Failed: unknown label "+label)
				}
			}
			issue := createIssue(repo, title, bodyText, labels)
			return restReply{Status: 201, Body: issueJSON(repo, issue)}
		}
		if method == "GET" {
			state, _ := req.queryGet("state")
			if !func() bool { _, present := req.Query["state"]; return present }() {
				state = "open"
			}
			direction, _ := req.queryGet("direction")
			list := []*issueRec{}
			for _, number := range repo.issueOrder {
				issue := repo.issues[number]
				if state == "open" || state == "closed" {
					if issue.State != state {
						continue
					}
				}
				list = append(list, issue)
			}
			sort.SliceStable(list, func(i, j int) bool {
				if direction == "asc" {
					return list[i].Number < list[j].Number
				}
				return list[i].Number > list[j].Number
			})
			items := make([]interface{}, 0, len(list))
			for _, issue := range list {
				items = append(items, issue)
			}
			page, headers := paginate(items, req.Query, "/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/issues", req.Host)
			body := make([]interface{}, 0, len(page))
			for _, item := range page {
				body = append(body, issueJSON(repo, item.(*issueRec)))
			}
			return restReply{Status: 200, Body: body, Headers: headers}
		}
		return restNotFound
	}

	number, parseOk := jsParseInt(seg[0])
	var issue *issueRec
	if parseOk {
		issue = repo.issues[int(number)]
	}

	if len(seg) == 1 {
		if issue == nil {
			return restNotFound
		}
		if method == "GET" {
			return restReply{Status: 200, Body: issueJSON(repo, issue)}
		}
		if method == "PATCH" {
			if state, ok := bodyString(req.Body, "state"); ok && (state == "closed" || state == "open") {
				issue.State = state
			}
			if title, ok := bodyString(req.Body, "title"); ok {
				issue.Title = title
			}
			if bodyText, ok := bodyString(req.Body, "body"); ok {
				issue.Body = bodyText
			}
			if labels, isArray := stringListField(req.Body, "labels"); isArray {
				for _, label := range labels {
					if _, exists := repo.labels[label]; !exists {
						return restMessage(422, "Validation Failed: unknown label "+label)
					}
				}
				issue.Labels = labels
			}
			return restReply{Status: 200, Body: issueJSON(repo, issue)}
		}
		return restNotFound
	}

	if len(seg) == 2 && seg[1] == "comments" {
		if issue == nil {
			return restNotFound
		}
		if method == "POST" {
			bodyText, ok := bodyString(req.Body, "body")
			if !ok {
				return restMessage(422, "Validation Failed: body is required")
			}
			comment := addComment(issue, bodyText)
			return restReply{Status: 201, Body: commentJSON(comment.Body, comment.ID, repo.Owner)}
		}
		if method == "GET" {
			out := []interface{}{}
			for _, comment := range issue.Comments {
				out = append(out, commentJSON(comment.Body, comment.ID, repo.Owner))
			}
			return restReply{Status: 200, Body: out}
		}
		return restNotFound
	}

	return restNotFound
}

// ---------------------------------------------------------------------------

func stripOwnerPrefix(ref string) string {
	if colon := strings.Index(ref, ":"); colon >= 0 {
		return ref[colon+1:]
	}
	return ref
}

func handlePulls(req *restRequest, repo *repoRec, seg []string) restReply {
	method := req.Method

	if len(seg) == 0 {
		if method == "POST" {
			title, _ := bodyString(req.Body, "title")
			bodyText, _ := bodyString(req.Body, "body")
			head := ""
			if value, ok := bodyString(req.Body, "head"); ok {
				head = stripOwnerPrefix(value)
			}
			base := ""
			if value, ok := bodyString(req.Body, "base"); ok {
				base = stripOwnerPrefix(value)
			}
			headSha, hasHead := repo.ref("refs/heads/" + head)
			if !hasHead {
				return restMessage(422, "Validation Failed: head branch "+head+" does not exist")
			}
			baseSha, hasBase := repo.ref("refs/heads/" + base)
			if !hasBase {
				return restMessage(422, "Validation Failed: base branch "+base+" does not exist")
			}
			for _, number := range repo.pullOrder {
				pull := repo.pulls[number]
				if pull.State == "open" && pull.HeadRef == head && pull.BaseRef == base {
					return restMessage(422, "Validation Failed: a pull request already exists for this branch combination")
				}
			}
			pull := createPull(repo, title, bodyText, head, base, headSha, baseSha)
			return restReply{Status: 201, Body: pullJSON(repo, pull)}
		}
		if method == "GET" {
			state, _ := req.queryGet("state")
			if _, present := req.Query["state"]; !present {
				state = "open"
			}
			list := []*pullRec{}
			for _, number := range repo.pullOrder {
				pull := repo.pulls[number]
				if state == "open" && pull.State != "open" {
					continue
				}
				if state == "closed" && pull.State != "closed" {
					continue
				}
				list = append(list, pull)
			}
			sort.SliceStable(list, func(i, j int) bool { return list[i].Number < list[j].Number })
			items := make([]interface{}, 0, len(list))
			for _, pull := range list {
				items = append(items, pull)
			}
			page, headers := paginate(items, req.Query, "/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/pulls", req.Host)
			body := make([]interface{}, 0, len(page))
			for _, item := range page {
				body = append(body, pullJSON(repo, item.(*pullRec)))
			}
			return restReply{Status: 200, Body: body, Headers: headers}
		}
		return restNotFound
	}

	number, parseOk := jsParseInt(seg[0])
	var pull *pullRec
	if parseOk {
		pull = repo.pulls[int(number)]
	}

	if len(seg) == 1 && method == "GET" {
		if pull == nil {
			return restNotFound
		}
		return restReply{Status: 200, Body: pullJSON(repo, pull)}
	}

	if len(seg) == 2 && seg[1] == "files" && method == "GET" {
		if pull == nil {
			return restNotFound
		}
		files := []interface{}{}
		for _, change := range pullChanges(repo, pull) {
			files = append(files, map[string]interface{}{
				"filename":  change.Path,
				"status":    change.Status,
				"additions": change.Additions,
				"deletions": change.Deletions,
				"changes":   change.Additions + change.Deletions,
			})
		}
		return restReply{Status: 200, Body: files}
	}

	if len(seg) == 2 && seg[1] == "merge" && method == "PUT" {
		if pull == nil {
			return restNotFound
		}
		if pull.State != "open" || pull.Merged {
			return restMessage(405, "Pull Request is not mergeable")
		}
		outcome := squashMerge(repo, pull)
		if !outcome.OK {
			if outcome.Reason == "conflict" {
				return restMessage(405, "Pull Request is not mergeable: merge conflict")
			}
			return restMessage(405, "Pull Request is not mergeable")
		}
		return restReply{Status: 200, Body: map[string]interface{}{
			"sha": outcome.Commit, "merged": true, "message": "Pull Request successfully merged",
		}}
	}

	return restNotFound
}
