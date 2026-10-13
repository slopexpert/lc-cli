// lc: minimal LeetCode CLI.
//
//	lc get <slug|url> [-l lang] [-o dir]   download problem, create boilerplate
//	lc test   [-t testcases.txt] [dir]     run solution against sample/custom tests
//	lc submit [dir]                        submit solution
//	lc langs  <slug|url>                   list available languages
//
// Auth: set LEETCODE_SESSION and LEETCODE_CSRF env vars (browser cookies
// "LEETCODE_SESSION" and "csrftoken"). Needed for test/submit.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
)

const base = "https://leetcode.com"

var extMap = map[string]string{
	"cpp": "cpp", "java": "java", "python": "py", "python3": "py", "c": "c",
	"csharp": "cs", "javascript": "js", "typescript": "ts", "php": "php",
	"swift": "swift", "kotlin": "kt", "dart": "dart", "golang": "go",
	"ruby": "rb", "scala": "scala", "rust": "rs", "racket": "rkt",
	"erlang": "erl", "elixir": "ex",
}

type snippet struct {
	Lang     string `json:"lang"`
	LangSlug string `json:"langSlug"`
	Code     string `json:"code"`
}

type question struct {
	QuestionID       string    `json:"questionId"`
	FrontendID       string    `json:"questionFrontendId"`
	Title            string    `json:"title"`
	TitleSlug        string    `json:"titleSlug"`
	Content          string    `json:"content"`
	Difficulty       string    `json:"difficulty"`
	ExampleTestcases string    `json:"exampleTestcases"`
	MetaData         string    `json:"metaData"`
	CodeSnippets     []snippet `json:"codeSnippets"`
}

type meta struct {
	Slug       string   `json:"slug"`
	QuestionID string   `json:"question_id"`
	Lang       string   `json:"lang"`
	File       string   `json:"file"`
	Params     []string `json:"params,omitempty"`
}

type client struct {
	http    *http.Client
	session string
	csrf    string
}

func newClient() *client {
	return &client{
		http:    &http.Client{Timeout: 30 * time.Second},
		session: os.Getenv("LEETCODE_SESSION"),
		csrf:    os.Getenv("LEETCODE_CSRF"),
	}
}

func (c *client) needAuth() error {
	if c.session == "" || c.csrf == "" {
		return errors.New("LEETCODE_SESSION and LEETCODE_CSRF env vars required")
	}
	return nil
}

func (c *client) do(method, path, slug string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) lc-cli")
	req.Header.Set("Origin", base)
	ref := base
	if slug != "" {
		ref = fmt.Sprintf("%s/problems/%s/", base, slug)
	}
	req.Header.Set("Referer", ref)
	if c.csrf != "" {
		req.Header.Set("x-csrftoken", c.csrf)
	}
	if c.session != "" || c.csrf != "" {
		req.Header.Set("Cookie", fmt.Sprintf("LEETCODE_SESSION=%s; csrftoken=%s", c.session, c.csrf))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, trunc(string(data), 300))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode: %w: %s", err, trunc(string(data), 300))
	}
	return nil
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

const questionQuery = `query q($titleSlug: String!) {
  question(titleSlug: $titleSlug) {
    questionId questionFrontendId title titleSlug content difficulty
    exampleTestcases metaData
    codeSnippets { lang langSlug code }
  }
}`

func (c *client) fetchQuestion(slug string) (*question, error) {
	var res struct {
		Data struct {
			Question *question `json:"question"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	body := map[string]any{
		"query":     questionQuery,
		"variables": map[string]string{"titleSlug": slug},
	}
	if err := c.do("POST", "/graphql", slug, body, &res); err != nil {
		return nil, err
	}
	if len(res.Errors) > 0 {
		return nil, errors.New(res.Errors[0].Message)
	}
	if res.Data.Question == nil {
		return nil, fmt.Errorf("problem %q not found", slug)
	}
	return res.Data.Question, nil
}

var (
	slugRe  = regexp.MustCompile(`problems/([a-z0-9-]+)`)
	supRe   = regexp.MustCompile(`(?s)<sup>(.*?)</sup>`)
	blankRe = regexp.MustCompile(`\n{3,}`)

	trailWSRe  = regexp.MustCompile(`(?m)[ \t]+$`)
	punctGapRe = regexp.MustCompile("([`*]) +([.,;])")
)

func parseSlug(s string) string {
	if m := slugRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return strings.Trim(strings.TrimSpace(s), "/")
}

func htmlToText(s string) string {
	// The library has no <sup> support; "10<sup>4</sup>" would become "104".
	s = supRe.ReplaceAllString(s, "^$1")
	md, err := htmltomarkdown.ConvertString(s)
	if err != nil {
		md = s
	}
	// Source HTML contains &nbsp;-only paragraphs. They become lines holding
	// a single space, which defeats blank-line collapsing, so strip first.
	md = strings.ReplaceAll(md, "\u00a0", " ")
	md = trailWSRe.ReplaceAllString(md, "")
	md = blankRe.ReplaceAllString(md, "\n\n")
	md = punctGapRe.ReplaceAllString(md, "$1$2") // "`x` ." -> "`x`."
	return strings.TrimSpace(md) + "\n"
}

func pickSnippet(q *question, lang string) (*snippet, error) {
	var avail []string
	for i := range q.CodeSnippets {
		sn := &q.CodeSnippets[i]
		if sn.LangSlug == lang {
			return sn, nil
		}
		avail = append(avail, sn.LangSlug)
	}
	sort.Strings(avail)
	return nil, fmt.Errorf("lang %q unavailable; options: %s", lang, strings.Join(avail, ", "))
}

// parseInterleaved parses flags that may appear before, between, or after
// positional arguments (the stdlib flag package stops at the first positional).
func parseInterleaved(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		fs.Parse(args)
		if fs.NArg() == 0 {
			return pos
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func cmdGet(args []string) error {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	lang := fs.String("l", "python3", "language slug (see `lc langs`)")
	out := fs.String("o", ".", "output base directory")
	pos := parseInterleaved(fs, args)
	if len(pos) != 1 {
		return errors.New("usage: lc get <slug|url> [-l lang] [-o dir]")
	}
	slug := parseSlug(pos[0])
	c := newClient()
	q, err := c.fetchQuestion(slug)
	if err != nil {
		return err
	}
	sn, err := pickSnippet(q, *lang)
	if err != nil {
		return err
	}
	ext := extMap[sn.LangSlug]
	if ext == "" {
		ext = "txt"
	}
	dir := filepath.Join(*out, fmt.Sprintf("%s-%s", q.FrontendID, q.TitleSlug))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	file := "solution." + ext
	readme := fmt.Sprintf("# %s. %s\n\n**Difficulty:** %s · [Problem link](%s/problems/%s/)\n\n%s",
		q.FrontendID, q.Title, q.Difficulty, base, q.TitleSlug, htmlToText(q.Content))
	m, _ := json.MarshalIndent(meta{Slug: q.TitleSlug, QuestionID: q.QuestionID, Lang: sn.LangSlug, File: file, Params: paramNames(q.MetaData)}, "", "  ")

	files := []struct {
		name, data string
		keep       bool // do not overwrite if exists
	}{
		{"README.md", readme, false},
		{"testcases.txt", q.ExampleTestcases + "\n", true},
		{"meta.json", string(m) + "\n", false},
		{file, sn.Code + "\n", true},
	}
	for _, f := range files {
		p := filepath.Join(dir, f.name)
		if _, err := os.Stat(p); err == nil && f.keep {
			fmt.Printf("skip (exists): %s\n", p)
			continue
		}
		if err := os.WriteFile(p, []byte(f.data), 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote: %s\n", p)
	}
	return nil
}

func cmdLangs(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: lc langs <slug|url>")
	}
	q, err := newClient().fetchQuestion(parseSlug(args[0]))
	if err != nil {
		return err
	}
	for _, s := range q.CodeSnippets {
		fmt.Printf("%-12s %s\n", s.LangSlug, s.Lang)
	}
	return nil
}

func loadMeta(dir string) (*meta, string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, "", fmt.Errorf("meta.json not found in %s: %w", dir, err)
	}
	var m meta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, "", err
	}
	if len(m.Params) == 0 { // meta.json from an older `get`: backfill names
		if q, err := newClient().fetchQuestion(m.Slug); err == nil {
			if m.Params = paramNames(q.MetaData); len(m.Params) > 0 {
				if nb, err := json.MarshalIndent(m, "", "  "); err == nil {
					os.WriteFile(filepath.Join(dir, "meta.json"), append(nb, '\n'), 0o644)
				}
			}
		}
	}
	code, err := os.ReadFile(filepath.Join(dir, m.File))
	if err != nil {
		return nil, "", err
	}
	return &m, string(code), nil
}

// paramNames extracts argument names from the problem's metaData JSON.
// Class/design problems have no "params" and yield nil.
func paramNames(raw string) []string {
	var md struct {
		Params []struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if json.Unmarshal([]byte(raw), &md) != nil {
		return nil
	}
	var out []string
	for _, p := range md.Params {
		out = append(out, p.Name)
	}
	return out
}

// splitCases groups input lines (one line per argument) into test cases.
// Returns nil when the line count does not divide evenly by len(params).
func splitCases(params []string, input string) [][]string {
	input = strings.TrimRight(strings.ReplaceAll(input, "\r\n", "\n"), "\n")
	if len(params) == 0 || input == "" {
		return nil
	}
	lines := strings.Split(input, "\n")
	if len(lines)%len(params) != 0 {
		return nil
	}
	var cases [][]string
	for i := 0; i < len(lines); i += len(params) {
		cases = append(cases, lines[i:i+len(params)])
	}
	return cases
}

func labelCase(params, vals []string, indent string) string {
	var sb strings.Builder
	for i, v := range vals {
		fmt.Fprintf(&sb, "%s%s = %s\n", indent, params[i], v)
	}
	return sb.String()
}

func printCases(params []string, input string, got, want []string) {
	cases := splitCases(params, input)
	if cases == nil || len(got) != len(cases) || len(want) != len(cases) {
		fmt.Printf("Output:   %s\n", strings.Join(got, " | "))
		fmt.Printf("Expected: %s\n", strings.Join(want, " | "))
		return
	}
	for i, c := range cases {
		verdict := "PASS"
		if got[i] != want[i] {
			verdict = "FAIL"
		}
		fmt.Printf("\nCase %d: %s\n%s", i+1, verdict, labelCase(params, c, "  "))
		fmt.Printf("  output   = %s\n  expected = %s\n", got[i], want[i])
	}
	fmt.Println()
}

// flex decodes a JSON string, array of strings, or null into a string.
// LeetCode returns the same field as a string (submit) or an array (test).
type flex string

func (f *flex) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flex(s)
		return nil
	}
	var l []string
	if err := json.Unmarshal(b, &l); err == nil {
		*f = flex(strings.Join(l, "\n"))
		return nil
	}
	*f = flex(strings.TrimSpace(string(b)))
	return nil
}

type checkResult struct {
	State            string   `json:"state"`
	StatusMsg        string   `json:"status_msg"`
	RunSuccess       bool     `json:"run_success"`
	CorrectAnswer    *bool    `json:"correct_answer"`
	CompileError     flex     `json:"compile_error"`
	FullCompileError flex     `json:"full_compile_error"`
	RuntimeError     flex     `json:"runtime_error"`
	FullRuntimeError flex     `json:"full_runtime_error"`
	StatusRuntime    flex     `json:"status_runtime"`
	StatusMemory     flex     `json:"status_memory"`
	TotalCorrect     int      `json:"total_correct"`
	TotalTestcases   int      `json:"total_testcases"`
	CodeAnswer       []string `json:"code_answer"`
	ExpectedCodeAns  []string `json:"expected_code_answer"`
	StdOutputList    []string `json:"std_output_list"`
	CodeOutput       flex     `json:"code_output"`
	ExpectedOutput   flex     `json:"expected_output"`
	LastTestcase     flex     `json:"last_testcase"`
	StdOutput        flex     `json:"std_output"`
}

func trimTrailingEmpty(l []string) []string {
	for len(l) > 0 && l[len(l)-1] == "" {
		l = l[:len(l)-1]
	}
	return l
}

func (c *client) poll(slug, id string) (*checkResult, error) {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		var r checkResult
		if err := c.do("GET", fmt.Sprintf("/submissions/detail/%s/check/", id), slug, nil, &r); err != nil {
			return nil, err
		}
		if r.State == "SUCCESS" {
			return &r, nil
		}
		time.Sleep(1 * time.Second)
	}
	return nil, errors.New("timed out waiting for result")
}

func printErrors(r *checkResult) bool {
	switch {
	case r.FullCompileError != "":
		fmt.Println("Compile error:\n" + string(r.FullCompileError))
	case r.CompileError != "":
		fmt.Println("Compile error:\n" + string(r.CompileError))
	case r.FullRuntimeError != "":
		fmt.Println("Runtime error:\n" + string(r.FullRuntimeError))
	case r.RuntimeError != "":
		fmt.Println("Runtime error:\n" + string(r.RuntimeError))
	default:
		return false
	}
	return true
}

func cmdTest(args []string) error {
	fs := flag.NewFlagSet("test", flag.ExitOnError)
	tc := fs.String("t", "", "testcases file (default: <dir>/testcases.txt)")
	pos := parseInterleaved(fs, args)
	dir := "."
	if len(pos) > 0 {
		dir = pos[0]
	}
	m, code, err := loadMeta(dir)
	if err != nil {
		return err
	}
	tcPath := *tc
	if tcPath == "" {
		tcPath = filepath.Join(dir, "testcases.txt")
	}
	tests, err := os.ReadFile(tcPath)
	if err != nil {
		return err
	}
	c := newClient()
	if err := c.needAuth(); err != nil {
		return err
	}
	var start struct {
		InterpretID string `json:"interpret_id"`
	}
	body := map[string]any{
		"data_input":  strings.TrimSpace(string(tests)),
		"lang":        m.Lang,
		"question_id": m.QuestionID,
		"typed_code":  code,
	}
	if err := c.do("POST", fmt.Sprintf("/problems/%s/interpret_solution/", m.Slug), m.Slug, body, &start); err != nil {
		return err
	}
	r, err := c.poll(m.Slug, start.InterpretID)
	if err != nil {
		return err
	}
	if printErrors(r) {
		os.Exit(1)
	}
	got := trimTrailingEmpty(r.CodeAnswer)
	want := trimTrailingEmpty(r.ExpectedCodeAns)
	fmt.Printf("Status:   %s\n", r.StatusMsg)
	fmt.Printf("Runtime:  %s\n", r.StatusRuntime)
	printCases(m.Params, string(tests), got, want)
	if s := strings.Join(trimTrailingEmpty(r.StdOutputList), " | "); s != "" {
		fmt.Printf("Stdout:   %s\n", s)
	}
	pass := r.RunSuccess
	if r.CorrectAnswer != nil {
		pass = pass && *r.CorrectAnswer
	} else {
		pass = pass && strings.Join(got, "\x00") == strings.Join(want, "\x00")
	}
	if !pass {
		fmt.Println("RESULT: FAIL")
		os.Exit(1)
	}
	fmt.Println("RESULT: PASS")
	return nil
}

func cmdSubmit(args []string) error {
	dir := "."
	if len(args) > 0 {
		dir = args[0]
	}
	m, code, err := loadMeta(dir)
	if err != nil {
		return err
	}
	c := newClient()
	if err := c.needAuth(); err != nil {
		return err
	}
	var start struct {
		SubmissionID int64 `json:"submission_id"`
	}
	body := map[string]any{
		"lang":        m.Lang,
		"question_id": m.QuestionID,
		"typed_code":  code,
	}
	if err := c.do("POST", fmt.Sprintf("/problems/%s/submit/", m.Slug), m.Slug, body, &start); err != nil {
		return err
	}
	r, err := c.poll(m.Slug, fmt.Sprint(start.SubmissionID))
	if err != nil {
		return err
	}
	if printErrors(r) {
		os.Exit(1)
	}
	fmt.Printf("Status:  %s\n", r.StatusMsg)
	fmt.Printf("Passed:  %d/%d\n", r.TotalCorrect, r.TotalTestcases)
	fmt.Printf("Runtime: %s\nMemory:  %s\n", r.StatusRuntime, r.StatusMemory)
	if r.StatusMsg != "Accepted" {
		if r.LastTestcase != "" {
			if cs := splitCases(m.Params, string(r.LastTestcase)); len(cs) == 1 {
				fmt.Printf("Failed input:\n%s", labelCase(m.Params, cs[0], "  "))
			} else {
				fmt.Printf("Failed input:\n%s\n", r.LastTestcase)
			}
		}
		fmt.Printf("Output:   %s\nExpected: %s\n", r.CodeOutput, r.ExpectedOutput)
		os.Exit(1)
	}
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  lc get <slug|url> [-l lang] [-o dir]
  lc langs <slug|url>
  lc test [-t file] [dir]
  lc submit [dir]

env: LEETCODE_SESSION, LEETCODE_CSRF (required for test/submit)`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	args := os.Args[2:]
	switch os.Args[1] {
	case "get":
		err = cmdGet(args)
	case "langs":
		err = cmdLangs(args)
	case "test":
		err = cmdTest(args)
	case "submit":
		err = cmdSubmit(args)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
