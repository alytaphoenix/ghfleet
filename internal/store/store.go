// Package store is the SQLite state for the ghfleet control plane.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alytaphoenix/ghfleet/internal/github"
	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA busy_timeout = 5000;

CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS prs (
  repo TEXT NOT NULL,
  number INTEGER NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL DEFAULT '',
  url TEXT NOT NULL DEFAULT '',
  author TEXT NOT NULL DEFAULT '',
  is_draft INTEGER NOT NULL DEFAULT 0,
  head_ref TEXT NOT NULL DEFAULT '',
  head_sha TEXT NOT NULL DEFAULT '',
  base_ref TEXT NOT NULL DEFAULT '',
  base_sha TEXT NOT NULL DEFAULT '',
  mergeable TEXT NOT NULL DEFAULT '',
  merge_state TEXT NOT NULL DEFAULT '',
  review_decision TEXT NOT NULL DEFAULT '',
  additions INTEGER NOT NULL DEFAULT 0,
  deletions INTEGER NOT NULL DEFAULT 0,
  changed_files INTEGER NOT NULL DEFAULT 0,
  comments_count INTEGER NOT NULL DEFAULT 0,
  check_state TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL DEFAULT '',
  last_seen_at TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (repo, number)
);

CREATE TABLE IF NOT EXISTS threads (
  id TEXT PRIMARY KEY,
  repo TEXT NOT NULL,
  pr_number INTEGER NOT NULL,
  path TEXT NOT NULL DEFAULT '',
  line INTEGER NOT NULL DEFAULT 0,
  is_resolved INTEGER NOT NULL DEFAULT 0,
  is_outdated INTEGER NOT NULL DEFAULT 0,
  last_comment_id TEXT NOT NULL DEFAULT '',
  last_comment_author TEXT NOT NULL DEFAULT '',
  handled_comment_id TEXT NOT NULL DEFAULT '',
  payload_json TEXT NOT NULL DEFAULT '{}',
  updated_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_threads_pr ON threads(repo, pr_number);

CREATE TABLE IF NOT EXISTS reviews (
  id TEXT PRIMARY KEY,
  repo TEXT NOT NULL,
  pr_number INTEGER NOT NULL,
  author TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL DEFAULT '',
  submitted_at TEXT NOT NULL DEFAULT '',
  handled_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_reviews_pr ON reviews(repo, pr_number);

CREATE TABLE IF NOT EXISTS checks (
  repo TEXT NOT NULL,
  pr_number INTEGER NOT NULL,
  name TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT '',
  conclusion TEXT NOT NULL DEFAULT '',
  details_url TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (repo, pr_number, name)
);

CREATE TABLE IF NOT EXISTS jobs (
  id TEXT PRIMARY KEY,
  repo TEXT NOT NULL,
  pr_number INTEGER NOT NULL DEFAULT 0,
  kind TEXT NOT NULL,
  head_sha TEXT NOT NULL DEFAULT '',
  base_sha TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  risk TEXT NOT NULL DEFAULT '',
  summary TEXT NOT NULL DEFAULT '',
  spec_json TEXT NOT NULL DEFAULT '{}',
  result_json TEXT NOT NULL DEFAULT '',
  cost_usd REAL NOT NULL DEFAULT 0,
  runner_id TEXT NOT NULL DEFAULT '',
  lease_until TEXT NOT NULL DEFAULT '',
  required_labels_json TEXT NOT NULL DEFAULT '[]',
  idempotency_key TEXT NOT NULL UNIQUE,
  attempts INTEGER NOT NULL DEFAULT 0,
  approved_at TEXT NOT NULL DEFAULT '',
  applied_at TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  started_at TEXT NOT NULL DEFAULT '',
  finished_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
CREATE INDEX IF NOT EXISTS idx_jobs_pr ON jobs(repo, pr_number, kind);

CREATE TABLE IF NOT EXISTS runners (
  id TEXT PRIMARY KEY,
  hostname TEXT NOT NULL DEFAULT '',
  os TEXT NOT NULL DEFAULT '',
  arch TEXT NOT NULL DEFAULT '',
  labels_json TEXT NOT NULL DEFAULT '[]',
  repos_json TEXT NOT NULL DEFAULT '[]',
  status TEXT NOT NULL DEFAULT 'online',
  version TEXT NOT NULL DEFAULT '',
  registered_at TEXT NOT NULL,
  last_seen TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS artifacts (
  job_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  path TEXT NOT NULL,
  size INTEGER NOT NULL DEFAULT 0,
  sha256 TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  PRIMARY KEY (job_id, kind)
);

CREATE TABLE IF NOT EXISTS actions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  target TEXT NOT NULL DEFAULT '',
  payload_json TEXT NOT NULL DEFAULT '{}',
  result TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
`

// Store wraps the SQLite database.
type Store struct {
	DB   *sql.DB
	Path string
}

// Open opens or creates the database.
func Open(path string) (*Store, error) {
	path = expand(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// Additive migrations for databases created by earlier builds.
	if _, err := db.Exec(`ALTER TABLE jobs ADD COLUMN cost_usd REAL NOT NULL DEFAULT 0`); err != nil &&
		!strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		_ = db.Close()
		return nil, fmt.Errorf("migrate cost_usd: %w", err)
	}
	return &Store{DB: db, Path: path}, nil
}

func expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return filepath.Clean(p)
}

// Close closes the database.
func (s *Store) Close() error { return s.DB.Close() }

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func tstring(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// PR is a stored pull request with its checks and threads.
type PR struct {
	github.PR
}

// UpsertPR stores a PR, its checks, and its review threads.
func (s *Store) UpsertPR(pr github.PR) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.Exec(`
INSERT INTO prs (repo, number, title, body, url, author, is_draft, head_ref, head_sha, base_ref, base_sha,
  mergeable, merge_state, review_decision, additions, deletions, changed_files, comments_count, check_state,
  created_at, updated_at, last_seen_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(repo, number) DO UPDATE SET
  title=excluded.title, body=excluded.body, url=excluded.url, author=excluded.author,
  is_draft=excluded.is_draft, head_ref=excluded.head_ref, head_sha=excluded.head_sha,
  base_ref=excluded.base_ref, base_sha=excluded.base_sha, mergeable=excluded.mergeable,
  merge_state=excluded.merge_state, review_decision=excluded.review_decision,
  additions=excluded.additions, deletions=excluded.deletions, changed_files=excluded.changed_files,
  comments_count=excluded.comments_count, check_state=excluded.check_state,
  created_at=excluded.created_at, updated_at=excluded.updated_at, last_seen_at=excluded.last_seen_at`,
		pr.Repo, pr.Number, pr.Title, pr.Body, pr.URL, pr.Author, boolInt(pr.IsDraft), pr.HeadRef, pr.HeadSHA,
		pr.BaseRef, pr.BaseSHA, pr.Mergeable, pr.MergeState, pr.ReviewDecision, pr.Additions, pr.Deletions,
		pr.ChangedFiles, pr.CommentsCount, pr.CheckState, tstring(pr.CreatedAt), tstring(pr.UpdatedAt), now())
	if err != nil {
		return fmt.Errorf("upsert pr: %w", err)
	}

	if _, err := tx.Exec(`DELETE FROM checks WHERE repo=? AND pr_number=?`, pr.Repo, pr.Number); err != nil {
		return err
	}
	for _, ch := range pr.Checks {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO checks (repo, pr_number, name, status, conclusion, details_url, updated_at)
			VALUES (?,?,?,?,?,?,?)`, pr.Repo, pr.Number, ch.Name, ch.Status, ch.Conclusion, ch.DetailsURL, now()); err != nil {
			return err
		}
	}

	for _, rv := range pr.Reviews {
		if _, err := tx.Exec(`
INSERT INTO reviews (id, repo, pr_number, author, state, body, submitted_at)
VALUES (?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET author=excluded.author, state=excluded.state, body=excluded.body,
  submitted_at=excluded.submitted_at`, rv.ID, pr.Repo, pr.Number, rv.Author, rv.State, rv.Body, tstring(rv.SubmittedAt)); err != nil {
			return err
		}
	}

	for _, th := range pr.Threads {
		payload, _ := json.Marshal(th)
		lastID, lastAuthor := "", ""
		if last, ok := th.LastComment(); ok {
			lastID, lastAuthor = last.ID, last.Author
		}
		if _, err := tx.Exec(`
INSERT INTO threads (id, repo, pr_number, path, line, is_resolved, is_outdated, last_comment_id, last_comment_author, payload_json, updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
  path=excluded.path, line=excluded.line, is_resolved=excluded.is_resolved, is_outdated=excluded.is_outdated,
  last_comment_id=excluded.last_comment_id, last_comment_author=excluded.last_comment_author,
  payload_json=excluded.payload_json, updated_at=excluded.updated_at`,
			th.ID, pr.Repo, pr.Number, th.Path, th.Line, boolInt(th.IsResolved), boolInt(th.IsOutdated),
			lastID, lastAuthor, string(payload), now()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ListPRs returns all PRs, newest first.
func (s *Store) ListPRs() ([]PR, error) {
	rows, err := s.DB.Query(`
SELECT repo, number, title, body, url, author, is_draft, head_ref, head_sha, base_ref, base_sha,
  mergeable, merge_state, review_decision, additions, deletions, changed_files, comments_count, check_state,
  created_at, updated_at
FROM prs ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	var out []PR
	for rows.Next() {
		var p github.PR
		var draft int
		var created, updated string
		if err := rows.Scan(&p.Repo, &p.Number, &p.Title, &p.Body, &p.URL, &p.Author, &draft, &p.HeadRef,
			&p.HeadSHA, &p.BaseRef, &p.BaseSHA, &p.Mergeable, &p.MergeState, &p.ReviewDecision, &p.Additions,
			&p.Deletions, &p.ChangedFiles, &p.CommentsCount, &p.CheckState, &created, &updated); err != nil {
			_ = rows.Close()
			return nil, err
		}
		p.IsDraft = draft != 0
		p.CreatedAt, p.UpdatedAt = parseTime(created), parseTime(updated)
		out = append(out, PR{PR: p})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	for i := range out {
		if err := s.loadChildren(&out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) loadChildren(pr *PR) error {
	rows, err := s.DB.Query(`SELECT name, status, conclusion, details_url FROM checks WHERE repo=? AND pr_number=? ORDER BY name`, pr.Repo, pr.Number)
	if err != nil {
		return err
	}
	for rows.Next() {
		var c github.Check
		if err := rows.Scan(&c.Name, &c.Status, &c.Conclusion, &c.DetailsURL); err != nil {
			_ = rows.Close()
			return err
		}
		pr.Checks = append(pr.Checks, c)
	}
	_ = rows.Close()

	trows, err := s.DB.Query(`SELECT payload_json FROM threads WHERE repo=? AND pr_number=? ORDER BY path, line`, pr.Repo, pr.Number)
	if err != nil {
		return err
	}
	defer func() { _ = trows.Close() }()
	for trows.Next() {
		var raw string
		if err := trows.Scan(&raw); err != nil {
			return err
		}
		var th github.Thread
		if err := json.Unmarshal([]byte(raw), &th); err == nil {
			pr.Threads = append(pr.Threads, th)
		}
	}
	return trows.Err()
}

// GetPR returns one stored PR.
func (s *Store) GetPR(repo string, number int) (PR, error) {
	var p github.PR
	var draft int
	var created, updated string
	err := s.DB.QueryRow(`
SELECT repo, number, title, body, url, author, is_draft, head_ref, head_sha, base_ref, base_sha,
  mergeable, merge_state, review_decision, additions, deletions, changed_files, comments_count, check_state,
  created_at, updated_at FROM prs WHERE repo=? AND number=?`, repo, number).
		Scan(&p.Repo, &p.Number, &p.Title, &p.Body, &p.URL, &p.Author, &draft, &p.HeadRef, &p.HeadSHA,
			&p.BaseRef, &p.BaseSHA, &p.Mergeable, &p.MergeState, &p.ReviewDecision, &p.Additions,
			&p.Deletions, &p.ChangedFiles, &p.CommentsCount, &p.CheckState, &created, &updated)
	if err != nil {
		return PR{}, err
	}
	p.IsDraft = draft != 0
	p.CreatedAt, p.UpdatedAt = parseTime(created), parseTime(updated)
	pr := PR{PR: p}
	if err := s.loadChildren(&pr); err != nil {
		return PR{}, err
	}
	return pr, nil
}

// PrunePRs removes PRs that are no longer in the observed set.
func (s *Store) PrunePRs(seen map[string]bool) error {
	rows, err := s.DB.Query(`SELECT repo, number, last_seen_at FROM prs`)
	if err != nil {
		return err
	}
	type key struct {
		repo   string
		number int
	}
	cutoff := time.Now().UTC().Add(-time.Hour)
	var stale []key
	for rows.Next() {
		var k key
		var lastSeen string
		if err := rows.Scan(&k.repo, &k.number, &lastSeen); err != nil {
			_ = rows.Close()
			return err
		}
		if seen[fmt.Sprintf("%s#%d", k.repo, k.number)] {
			continue
		}
		// Keep PRs around briefly so a search-index hiccup does not wipe state.
		if t := parseTime(lastSeen); t.IsZero() || t.Before(cutoff) {
			stale = append(stale, k)
		}
	}
	_ = rows.Close()
	for _, k := range stale {
		_, _ = s.DB.Exec(`DELETE FROM threads WHERE repo=? AND pr_number=?`, k.repo, k.number)
		_, _ = s.DB.Exec(`DELETE FROM reviews WHERE repo=? AND pr_number=?`, k.repo, k.number)
		_, _ = s.DB.Exec(`DELETE FROM checks WHERE repo=? AND pr_number=?`, k.repo, k.number)
		_, _ = s.DB.Exec(`DELETE FROM prs WHERE repo=? AND number=?`, k.repo, k.number)
	}
	return nil
}

// UnresolvedThreads returns unresolved threads whose latest comment has not been handled.
func (s *Store) UnresolvedThreads(repo string, number int, viewer string) ([]github.Thread, error) {
	rows, err := s.DB.Query(`
SELECT payload_json FROM threads
WHERE repo=? AND pr_number=? AND is_resolved=0 AND is_outdated=0
  AND (handled_comment_id='' OR handled_comment_id != last_comment_id)
ORDER BY path, line`, repo, number)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []github.Thread
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var th github.Thread
		if err := json.Unmarshal([]byte(raw), &th); err != nil {
			continue
		}
		if th.NeedsAttention(viewer) {
			out = append(out, th)
		}
	}
	return out, rows.Err()
}

// UnhandledReviews returns the latest CHANGES_REQUESTED review per author that
// has not been handled yet.
func (s *Store) UnhandledReviews(repo string, number int, viewer string) ([]github.Review, error) {
	rows, err := s.DB.Query(`
SELECT r.id, r.author, r.body, r.state, r.submitted_at
FROM reviews r
WHERE r.repo=? AND r.pr_number=? AND r.state='CHANGES_REQUESTED' AND r.handled_at=''
  AND r.submitted_at = (
    SELECT MAX(r2.submitted_at) FROM reviews r2
    WHERE r2.repo=r.repo AND r2.pr_number=r.pr_number AND r2.author=r.author
  )
ORDER BY r.submitted_at`, repo, number)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []github.Review
	for rows.Next() {
		var rv github.Review
		var submitted string
		if err := rows.Scan(&rv.ID, &rv.Author, &rv.Body, &rv.State, &submitted); err != nil {
			return nil, err
		}
		if rv.Author == viewer || strings.HasSuffix(rv.Author, "[bot]") {
			continue
		}
		rv.SubmittedAt = parseTime(submitted)
		out = append(out, rv)
	}
	return out, rows.Err()
}

// MarkReviewsHandled marks reviews as handled.
func (s *Store) MarkReviewsHandled(ids []string) error {
	for _, id := range ids {
		if _, err := s.DB.Exec(`UPDATE reviews SET handled_at=? WHERE id=?`, now(), id); err != nil {
			return err
		}
	}
	return nil
}

// MarkThreadHandled marks the current latest comment in a thread as handled.
func (s *Store) MarkThreadHandled(threadID string) error {
	_, err := s.DB.Exec(`UPDATE threads SET handled_comment_id = last_comment_id WHERE id=?`, threadID)
	return err
}

// MarkThreadHandledAt marks a specific comment as handled. This avoids
// swallowing a newer comment that arrived while the job was running.
func (s *Store) MarkThreadHandledAt(threadID, commentID string) error {
	_, err := s.DB.Exec(`UPDATE threads SET handled_comment_id=? WHERE id=?`, commentID, threadID)
	return err
}

// MarkThreadsHandled marks several threads as handled at their current latest comment.
func (s *Store) MarkThreadsHandled(threadIDs []string) error {
	for _, id := range threadIDs {
		if err := s.MarkThreadHandled(id); err != nil {
			return err
		}
	}
	return nil
}

// HasActiveJob reports whether a repo/PR/kind already has an in-flight job.
func (s *Store) HasActiveJob(repo string, prNumber int, kind string) (bool, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE repo=? AND pr_number=? AND kind=? AND status IN
		('pending','claimed','running','awaiting_approval','approved')`, repo, prNumber, kind).Scan(&n)
	return n > 0, err
}

// CountJobs counts jobs for a repo/PR/kind, optionally since a time.
func (s *Store) CountJobs(repo string, prNumber int, kind string, since time.Time) (int, error) {
	var n int
	query := `SELECT COUNT(*) FROM jobs WHERE repo=? AND pr_number=? AND kind=?`
	args := []any{repo, prNumber, kind}
	if !since.IsZero() {
		query += ` AND created_at >= ?`
		args = append(args, tstring(since))
	}
	err := s.DB.QueryRow(query, args...).Scan(&n)
	return n, err
}

// ReleaseKey frees a job's idempotency key so a retry can be enqueued while keeping the audit row.
func (s *Store) ReleaseKey(id string) error {
	_, err := s.DB.Exec(`UPDATE jobs SET idempotency_key = idempotency_key || ':closed:' || id WHERE id=?`, id)
	return err
}

// CountClosedJobs counts failed or denied jobs for a repo/PR/kind.
func (s *Store) CountClosedJobs(repo string, prNumber int, kind string) (int, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE repo=? AND pr_number=? AND kind=? AND status IN ('failed','denied')`,
		repo, prNumber, kind).Scan(&n)
	return n, err
}

// Job is one unit of work.
type Job struct {
	ID             string
	Repo           string
	PRNumber       int
	Kind           string
	HeadSHA        string
	BaseSHA        string
	Status         string
	Risk           string
	Summary        string
	SpecJSON       string
	ResultJSON     string
	CostUSD        float64
	RunnerID       string
	LeaseUntil     time.Time
	RequiredLabels []string
	IdempotencyKey string
	Attempts       int
	ApprovedAt     time.Time
	AppliedAt      time.Time
	CreatedAt      time.Time
	StartedAt      time.Time
	FinishedAt     time.Time
	Error          string
}

// Enqueue inserts a job if its idempotency key is new. It returns true when inserted.
func (s *Store) Enqueue(j Job) (bool, error) {
	if j.CreatedAt.IsZero() {
		j.CreatedAt = time.Now()
	}
	labels, _ := json.Marshal(j.RequiredLabels)
	insert := func(id string) (bool, error) {
		res, err := s.DB.Exec(`
INSERT OR IGNORE INTO jobs (id, repo, pr_number, kind, head_sha, base_sha, status, risk, summary, spec_json,
  result_json, runner_id, lease_until, required_labels_json, idempotency_key, attempts, created_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, j.Repo, j.PRNumber, j.Kind, j.HeadSHA, j.BaseSHA, j.Status, j.Risk, j.Summary, j.SpecJSON,
			j.ResultJSON, j.RunnerID, tstring(j.LeaseUntil), string(labels), j.IdempotencyKey, j.Attempts, tstring(j.CreatedAt))
		if err != nil {
			return false, err
		}
		n, _ := res.RowsAffected()
		return n > 0, nil
	}
	ok, err := insert(j.ID)
	if err != nil || ok {
		return ok, err
	}
	// A failed/denied retry keeps the old row for audit, but its deterministic
	// ID can collide. If the idempotency key is free, insert under a retry ID.
	var existingKey string
	if err := s.DB.QueryRow(`SELECT idempotency_key FROM jobs WHERE id=?`, j.ID).Scan(&existingKey); err == nil &&
		existingKey != j.IdempotencyKey {
		retryID := fmt.Sprintf("%s-r%d", j.ID, time.Now().UTC().Unix())
		return insert(retryID)
	}
	return false, nil
}

const jobColumns = `id, repo, pr_number, kind, head_sha, base_sha, status, risk, summary, spec_json, result_json,
 cost_usd, runner_id, lease_until, required_labels_json, idempotency_key, attempts, approved_at, applied_at,
 created_at, started_at, finished_at, error`

func scanJob(sc interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var lease, approved, applied, created, started, finished string
	var labels string
	err := sc.Scan(&j.ID, &j.Repo, &j.PRNumber, &j.Kind, &j.HeadSHA, &j.BaseSHA, &j.Status, &j.Risk, &j.Summary,
		&j.SpecJSON, &j.ResultJSON, &j.CostUSD, &j.RunnerID, &lease, &labels, &j.IdempotencyKey, &j.Attempts,
		&approved, &applied, &created, &started, &finished, &j.Error)
	if err != nil {
		return j, err
	}
	j.LeaseUntil = parseTime(lease)
	j.ApprovedAt = parseTime(approved)
	j.AppliedAt = parseTime(applied)
	j.CreatedAt = parseTime(created)
	j.StartedAt = parseTime(started)
	j.FinishedAt = parseTime(finished)
	_ = json.Unmarshal([]byte(labels), &j.RequiredLabels)
	return j, nil
}

// GetJob returns a job by ID.
func (s *Store) GetJob(id string) (Job, error) {
	row := s.DB.QueryRow(`SELECT `+jobColumns+` FROM jobs WHERE id=?`, id)
	return scanJob(row)
}

// ListJobs returns jobs in the given statuses, newest first.
func (s *Store) ListJobs(statuses []string, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 100
	}
	query := `SELECT ` + jobColumns + ` FROM jobs`
	var args []any
	if len(statuses) > 0 {
		query += ` WHERE status IN (` + placeholders(len(statuses)) + `)`
		for _, st := range statuses {
			args = append(args, st)
		}
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// PendingJobs returns unleased pending jobs.
func (s *Store) PendingJobs(limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.DB.Query(`SELECT `+jobColumns+` FROM jobs WHERE status='pending' ORDER BY created_at LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ClaimJob atomically moves a pending job to claimed.
func (s *Store) ClaimJob(id, runnerID string, lease time.Time) (bool, error) {
	res, err := s.DB.Exec(`
UPDATE jobs SET status='claimed', runner_id=?, lease_until=?, started_at=CASE WHEN started_at='' THEN ? ELSE started_at END,
  attempts=attempts+1
WHERE id=? AND status='pending'`, runnerID, tstring(lease), now(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ReserveApproved leases an approved job to one runner so concurrent workers do
// not apply it twice. It returns false when another runner holds the lease.
func (s *Store) ReserveApproved(id, runnerID string, lease time.Time) (bool, error) {
	res, err := s.DB.Exec(`
UPDATE jobs SET runner_id=?, lease_until=?
WHERE id=? AND status='approved' AND (runner_id=? OR lease_until='' OR lease_until < ?)`,
		runnerID, tstring(lease), id, runnerID, now())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetJobStatus updates status and optionally error.
func (s *Store) SetJobStatus(id, status, errText string) error {
	_, err := s.DB.Exec(`UPDATE jobs SET status=?, error=?, finished_at=CASE WHEN ? IN ('failed','applied','denied','cancelled') THEN ? ELSE finished_at END WHERE id=?`,
		status, errText, status, now(), id)
	return err
}

// SaveResult stores an agent result and moves the job to awaiting_approval.
// leaseUntil keeps other workers from grabbing the job while the original
// worker waits for approval.
func (s *Store) SaveResult(id, resultJSON, risk, summary string, costUSD float64, leaseUntil time.Time) error {
	if leaseUntil.IsZero() {
		leaseUntil = time.Now().Add(24 * time.Hour)
	}
	_, err := s.DB.Exec(`UPDATE jobs SET status='awaiting_approval', result_json=?, risk=?, summary=?, cost_usd=?, lease_until=? WHERE id=?`,
		resultJSON, risk, summary, costUSD, tstring(leaseUntil), id)
	if err != nil {
		return err
	}
	return s.Action(id, "result", "", summary)
}

// Approve marks a job approved.
func (s *Store) Approve(id string) error {
	lease := time.Now().UTC().Add(24 * time.Hour)
	res, err := s.DB.Exec(`UPDATE jobs SET status='approved', approved_at=?,
		lease_until=CASE WHEN lease_until='' OR lease_until < ? THEN ? ELSE lease_until END
		WHERE id=? AND status='awaiting_approval'`, now(), now(), tstring(lease), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("job %s is not awaiting approval", id)
	}
	return s.Action(id, "approve", "", "")
}

// Deny marks a job denied.
func (s *Store) Deny(id string) error {
	res, err := s.DB.Exec(`UPDATE jobs SET status='denied', finished_at=? WHERE id=? AND status IN ('awaiting_approval','approved','pending','claimed','running')`, now(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("job %s cannot be denied in its current state", id)
	}
	return s.Action(id, "deny", "", "")
}

// MarkApplied marks a job applied and stores the final result.
func (s *Store) MarkApplied(id, resultJSON string) error {
	_, err := s.DB.Exec(`UPDATE jobs SET status='applied', result_json=?, applied_at=?, finished_at=? WHERE id=?`,
		resultJSON, now(), now(), id)
	if err != nil {
		return err
	}
	return s.Action(id, "applied", "", "")
}

// CountStartedSince counts jobs that started after a time.
func (s *Store) CountStartedSince(since time.Time) (int, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE started_at >= ?`, tstring(since)).Scan(&n)
	return n, err
}

// CostSince sums agent cost for jobs started after a time.
func (s *Store) CostSince(since time.Time) (float64, error) {
	var v float64
	err := s.DB.QueryRow(`SELECT COALESCE(SUM(cost_usd),0) FROM jobs WHERE started_at >= ?`, tstring(since)).Scan(&v)
	return v, err
}

// RequeueExpired returns expired leases to the pending queue.
func (s *Store) RequeueExpired() (int64, error) {
	res, err := s.DB.Exec(`
UPDATE jobs SET status='pending', runner_id='', lease_until=''
WHERE status IN ('claimed','running') AND lease_until != '' AND lease_until < ?`, now())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Instruction is the next action for a runner.
type Instruction struct {
	Action string `json:"action"` // wait | apply | deny
	Job    Job    `json:"job"`
}

// InstructionFor returns the next action for a runner-owned job.
func (s *Store) InstructionFor(id, runnerID string) (Instruction, error) {
	j, err := s.GetJob(id)
	if err != nil {
		return Instruction{}, err
	}
	if j.RunnerID != runnerID {
		return Instruction{}, fmt.Errorf("job %s belongs to another runner", id)
	}
	switch j.Status {
	case "approved":
		return Instruction{Action: "apply", Job: j}, nil
	case "denied", "cancelled":
		return Instruction{Action: "deny", Job: j}, nil
	case "applied", "failed":
		return Instruction{Action: "deny", Job: j}, nil
	default:
		return Instruction{Action: "wait", Job: j}, nil
	}
}

// JobsForRunner returns jobs owned by a runner in the given statuses.
func (s *Store) JobsForRunner(runnerID string, statuses []string, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 20
	}
	query := `SELECT ` + jobColumns + ` FROM jobs WHERE runner_id=?`
	args := []any{runnerID}
	if len(statuses) > 0 {
		query += ` AND status IN (` + placeholders(len(statuses)) + `)`
		for _, st := range statuses {
			args = append(args, st)
		}
	}
	query += ` ORDER BY created_at LIMIT ?`
	args = append(args, limit)
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Artifact describes an uploaded file.
type Artifact struct {
	JobID     string
	Kind      string
	Path      string
	Size      int64
	SHA256    string
	CreatedAt time.Time
}

// SetArtifact records an artifact.
func (s *Store) SetArtifact(jobID, kind, path string, size int64, sha string) error {
	_, err := s.DB.Exec(`INSERT OR REPLACE INTO artifacts (job_id, kind, path, size, sha256, created_at) VALUES (?,?,?,?,?,?)`,
		jobID, kind, path, size, sha, now())
	return err
}

// GetArtifact returns an artifact.
func (s *Store) GetArtifact(jobID, kind string) (Artifact, error) {
	var a Artifact
	var created string
	err := s.DB.QueryRow(`SELECT job_id, kind, path, size, sha256, created_at FROM artifacts WHERE job_id=? AND kind=?`, jobID, kind).
		Scan(&a.JobID, &a.Kind, &a.Path, &a.Size, &a.SHA256, &created)
	if err != nil {
		return a, err
	}
	a.CreatedAt = parseTime(created)
	return a, nil
}

// Runner is a registered runner.
type Runner struct {
	ID           string
	Hostname     string
	OS           string
	Arch         string
	Labels       []string
	Repos        []string
	Status       string
	Version      string
	RegisteredAt time.Time
	LastSeen     time.Time
}

// UpsertRunner registers or updates a runner.
func (s *Store) UpsertRunner(r Runner) error {
	labels, _ := json.Marshal(r.Labels)
	repos, _ := json.Marshal(r.Repos)
	_, err := s.DB.Exec(`
INSERT INTO runners (id, hostname, os, arch, labels_json, repos_json, status, version, registered_at, last_seen)
VALUES (?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET hostname=excluded.hostname, os=excluded.os, arch=excluded.arch,
  labels_json=excluded.labels_json, repos_json=excluded.repos_json, status=excluded.status,
  version=excluded.version, last_seen=excluded.last_seen`,
		r.ID, r.Hostname, r.OS, r.Arch, string(labels), string(repos), "online", r.Version, now(), now())
	if err != nil {
		return err
	}
	return nil
}

// Heartbeat updates a runner's last_seen and state.
func (s *Store) Heartbeat(id string) error {
	_, err := s.DB.Exec(`UPDATE runners SET last_seen=?, status='online' WHERE id=?`, now(), id)
	return err
}

// ListRunners returns all runners.
func (s *Store) ListRunners() ([]Runner, error) {
	rows, err := s.DB.Query(`SELECT id, hostname, os, arch, labels_json, repos_json, status, version, registered_at, last_seen FROM runners ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Runner
	for rows.Next() {
		var r Runner
		var labels, repos, reg, seen string
		if err := rows.Scan(&r.ID, &r.Hostname, &r.OS, &r.Arch, &labels, &repos, &r.Status, &r.Version, &reg, &seen); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(labels), &r.Labels)
		_ = json.Unmarshal([]byte(repos), &r.Repos)
		r.RegisteredAt, r.LastSeen = parseTime(reg), parseTime(seen)
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkStaleRunners marks runners offline when they miss heartbeats.
func (s *Store) MarkStaleRunners(cutoff time.Time) (int64, error) {
	res, err := s.DB.Exec(`UPDATE runners SET status='offline' WHERE status='online' AND last_seen < ?`, tstring(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Action records an audit event.
func (s *Store) Action(jobID, kind, target, result string) error {
	_, err := s.DB.Exec(`INSERT INTO actions (job_id, kind, target, payload_json, result, created_at) VALUES (?,?,?,?,?,?)`,
		jobID, kind, target, "{}", result, now())
	return err
}

// Stats is a small dashboard summary.
type Stats struct {
	PRs                  int       `json:"prs"`
	JobsPending          int       `json:"jobs_pending"`
	JobsRunning          int       `json:"jobs_running"`
	JobsAwaitingApproval int       `json:"jobs_awaiting_approval"`
	JobsApproved         int       `json:"jobs_approved"`
	JobsApplied          int       `json:"jobs_applied"`
	JobsFailed           int       `json:"jobs_failed"`
	RunnersOnline        int       `json:"runners_online"`
	RunnersTotal         int       `json:"runners_total"`
	ThreadsOpen          int       `json:"threads_open"`
	CostUSD24h           float64   `json:"cost_usd_24h"`
	LastScan             time.Time `json:"last_scan"`
}

// Stats computes dashboard counts.
func (s *Store) Stats() (Stats, error) {
	var st Stats
	counts := map[string]*int{
		"pending": &st.JobsPending, "claimed": &st.JobsRunning, "running": &st.JobsRunning,
		"awaiting_approval": &st.JobsAwaitingApproval, "approved": &st.JobsApproved,
		"applied": &st.JobsApplied, "failed": &st.JobsFailed,
	}
	for status, dst := range counts {
		var n int
		if err := s.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status=?`, status).Scan(&n); err != nil {
			return st, err
		}
		if status == "claimed" || status == "running" {
			*dst += n
		} else {
			*dst = n
		}
	}
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM prs`).Scan(&st.PRs)
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM runners WHERE status='online'`).Scan(&st.RunnersOnline)
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM runners`).Scan(&st.RunnersTotal)
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM threads WHERE is_resolved=0 AND is_outdated=0`).Scan(&st.ThreadsOpen)
	st.CostUSD24h, _ = s.CostSince(time.Now().Add(-24 * time.Hour))
	st.LastScan = s.MetaTime("last_scan")
	return st, nil
}

// SetMeta stores a string value.
func (s *Store) SetMeta(key, value string) error {
	_, err := s.DB.Exec(`INSERT INTO meta (key, value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// MetaTime reads a timestamp value.
func (s *Store) MetaTime(key string) time.Time {
	var v string
	if err := s.DB.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v); err != nil {
		return time.Time{}
	}
	return parseTime(v)
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
