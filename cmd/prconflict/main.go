// prconflict v0.4 – GraphQL‑powered: insert only unresolved PR threads as conflict markers
//
// Highlights
//   - Uses GitHub GraphQL v4 API to discover threads where `isResolved == false`
//   - Fetches comment details via REST (go‑github v72) to get path/line mapping
//   - Groups comments by file & line, preserving chronological order
//   - Generates Git‑style conflict blocks for each unresolved thread
//
// Build & Run
//
//	GITHUB_TOKEN=<pat> gh pr checkout <PR#>
//	go run ./prconflict --repo owner/repo --pr <PR#> [--dry-run]
//
// Requirements
//   - Go 1.21+
//   - github.com/google/go-github/v72
//   - github.com/shurcooL/githubv4 (GraphQL client)
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v72/github"
	"github.com/shurcooL/githubv4"
	"golang.org/x/oauth2"
)

// commentInfo holds minimal data for a review comment.
type commentInfo struct {
	id      int64
	user    string
	body    string
	created time.Time
}

type lineThread struct {
	line     int
	comments []commentInfo
}

func main() {
	repoFlag := flag.String("repo", "", "GitHub repo in owner/name format (optional, autodetected)")
	prNum := flag.Int("pr", 0, "Pull request number (optional, autodetected)")
	branchFlag := flag.String("branch", "", "Git branch name for PR detection (optional)")
	dryRun := flag.Bool("dry-run", false, "Print changes instead of writing files")
	flag.Parse()

	// Determine repository (owner/repo)
	repoVal := *repoFlag
	if repoVal == "" {
		out, err := exec.Command("gh", "repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner").Output()
		if err != nil {
			log.Fatalf("could not detect repository: %v", err)
		}
		repoVal = strings.TrimSpace(string(out))
	}

	// Determine PR number
	prNumVal := *prNum
	if prNumVal == 0 {
		if *branchFlag != "" {
			out, err := exec.Command("gh", "pr", "list", "--json", "number", "--head", *branchFlag).Output()
			if err != nil {
				log.Fatalf("could not detect PR number from branch %s: %v", *branchFlag, err)
			}
			var prs []struct{ Number int }
			if err := json.Unmarshal(out, &prs); err != nil {
				log.Fatalf("invalid JSON from gh pr list: %v", err)
			}
			if len(prs) == 0 {
				log.Fatalf("no PR found for branch %s", *branchFlag)
			}
			prNumVal = prs[0].Number
		} else {
			out, err := exec.Command("gh", "pr", "view", "--json", "number", "--jq", ".number").Output()
			if err != nil {
				log.Fatalf("could not detect PR number: %v", err)
			}
			num, err := strconv.Atoi(strings.TrimSpace(string(out)))
			if err != nil {
				log.Fatalf("invalid PR number from gh CLI: %v", err)
			}
			prNumVal = num
		}
	}

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		log.Fatal("GITHUB_TOKEN env var missing – provide a PAT with repo scope")
	}

	owner, repo, ok := splitRepo(repoVal)
	if !ok {
		log.Fatalf("invalid repository format: %s", repoVal)
	}

	// OAuth‑backed HTTP client for both REST and GraphQL
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	httpClient := oauth2.NewClient(ctx, ts)

	ghREST := github.NewClient(httpClient)
	ghQL := githubv4.NewClient(httpClient)

	// 1. Get IDs of comments in unresolved threads via GraphQL
	unresolvedIDs := getUnresolvedCommentIDs(ctx, ghQL, owner, repo, prNumVal)
	if len(unresolvedIDs) == 0 {
		log.Println("All review threads resolved – nothing to do.")
		return
	}

	// 2. Fetch *all* review comments via REST (cheap) and keep only unresolved ones
	comments := fetchReviewComments(ctx, ghREST, owner, repo, prNumVal)

	fileThreads := map[string]map[int]*lineThread{}
	for _, c := range comments {
		if c.Path == nil || c.Line == nil {
			continue // outdated
		}
		if _, keep := unresolvedIDs[c.GetID()]; !keep {
			continue // resolved – skip
		}
		path := c.GetPath()
		ln := c.GetLine()
		if fileThreads[path] == nil {
			fileThreads[path] = map[int]*lineThread{}
		}
		if fileThreads[path][ln] == nil {
			fileThreads[path][ln] = &lineThread{line: ln}
		}
		fileThreads[path][ln].comments = append(fileThreads[path][ln].comments, commentInfo{
			id:      c.GetID(),
			user:    nonEmpty(c.GetUser().GetLogin()),
			body:    nonEmpty(c.GetBody()),
			created: c.GetCreatedAt().Time,
		})
	}

	if len(fileThreads) == 0 {
		log.Println("No unresolved comments align with current lines – finished.")
		return
	}

	// 3. Inject conflict blocks
	for path, lineMap := range fileThreads {
		var threads []lineThread
		for _, t := range lineMap {
			sort.Slice(t.comments, func(i, j int) bool {
				return t.comments[i].created.Before(t.comments[j].created)
			})
			threads = append(threads, *t)
		}
		sort.Slice(threads, func(i, j int) bool { return threads[i].line > threads[j].line })
		if err := injectThreads(path, threads, *dryRun); err != nil {
			log.Printf("%s: %v", path, err)
		}
	}
}

// getUnresolvedCommentIDs queries GraphQL v4 for unresolved threads and returns their comment DB IDs.
func getUnresolvedCommentIDs(ctx context.Context, client *githubv4.Client, owner, repo string, prNumber int) map[int64]struct{} {
	type commentNode struct {
		DatabaseID githubv4.Int `graphql:"databaseId"`
	}
	var q struct {
		Repository struct {
			PullRequest struct {
				ReviewThreads struct {
					PageInfo struct {
						HasNextPage githubv4.Boolean
						EndCursor   githubv4.String
					}
					Nodes []struct {
						IsResolved githubv4.Boolean
						Comments   struct {
							Nodes []commentNode
						} `graphql:"comments(first: 100)"`
					}
				} `graphql:"reviewThreads(first: 100, after: $cursor)"`
			} `graphql:"pullRequest(number: $pr)"`
		} `graphql:"repository(owner: $owner, name: $name)"`
	}
	vars := map[string]interface{}{
		"owner":  githubv4.String(owner),
		"name":   githubv4.String(repo),
		"pr":     githubv4.Int(prNumber),
		"cursor": (*githubv4.String)(nil),
	}

	ids := make(map[int64]struct{})

	for {
		if err := client.Query(ctx, &q, vars); err != nil {
			log.Fatalf("GraphQL query: %v", err)
		}
		for _, th := range q.Repository.PullRequest.ReviewThreads.Nodes {
			if bool(th.IsResolved) {
				continue
			}
			for _, c := range th.Comments.Nodes {
				ids[int64(c.DatabaseID)] = struct{}{}
			}
		}
		if !bool(q.Repository.PullRequest.ReviewThreads.PageInfo.HasNextPage) {
			break
		}
		vars["cursor"] = githubv4.NewString(q.Repository.PullRequest.ReviewThreads.PageInfo.EndCursor)
	}
	return ids
}

// fetchReviewComments uses REST to obtain path & line info for all comments.
func fetchReviewComments(ctx context.Context, gh *github.Client, owner, repo string, pr int) []*github.PullRequestComment {
	var all []*github.PullRequestComment
	opts := &github.PullRequestListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		cs, resp, err := gh.PullRequests.ListComments(ctx, owner, repo, pr, opts)
		if err != nil {
			log.Fatalf("ListComments: %v", err)
		}
		all = append(all, cs...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all
}

// injectThreads writes review conflict blocks into a file.
func injectThreads(path string, threads []lineThread, dry bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var src []string
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		src = append(src, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return err
	}

	for _, th := range threads {
		idx := th.line - 1
		if idx < 0 || idx >= len(src) {
			log.Printf("%s:%d – line vanished, skipping", path, th.line)
			continue
		}
		block := buildBlock(th.comments)
		trailer := ">>>>>>> END REVIEW"
		insertion := append(block, append([]string{src[idx]}, trailer)...)
		src = append(src[:idx], append(insertion, src[idx+1:]...)...)
	}

	if dry {
		fmt.Printf("--- %s (dry-run)\n", path)
		for i, l := range src {
			fmt.Printf("%6d %s\n", i+1, l)
		}
		return nil
	}

	return os.WriteFile(path, []byte(strings.Join(src, "\n")+"\n"), 0644)
}

func buildBlock(cs []commentInfo) []string {
	header := fmt.Sprintf("<<<<<<< REVIEW THREAD (%d)", len(cs))
	lines := []string{header}
	for _, c := range cs {
		ts := c.created.Format("2006-01-02 15:04")
		lines = append(lines, fmt.Sprintf("%s %s: %s", ts, c.user, sanitize(c.body)))
	}
	lines = append(lines, "=======")
	return lines
}

// helper utilities
func splitRepo(s string) (string, string, bool) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func sanitize(s string) string {
	repl := strings.NewReplacer("\n", " ", "\r", " ", "*", "", "/", "")
	return strings.TrimSpace(repl.Replace(s))
}

func nonEmpty(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}
