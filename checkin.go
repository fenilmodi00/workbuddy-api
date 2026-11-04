package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const checkinStateFile = "checkin-state.json"

var checkinZone = time.FixedZone("UTC+8", 8*60*60)
var dailyCheckins = newDailyCheckinScheduler(checkinStateFile)

type checkinStatusSnapshot struct {
	Date     string `json:"date"`
	Status   string `json:"status"` // pending/running/success/failed/missed/unsupported/unknown
	Attempts int    `json:"attempts"`
	NextAt   int64  `json:"nextAt,omitempty"`
	LastAt   int64  `json:"lastAt,omitempty"`
	Message  string `json:"message,omitempty"`
}

// Only hashes and fixed status messages are saved; never save UID/token/nickname.
type checkinRecord struct {
	FirstAt   int64  `json:"firstAt"`
	RetryAt   int64  `json:"retryAt"`
	StartupAt int64  `json:"startupAt,omitempty"`
	Attempts  int    `json:"attempts"`
	Status    string `json:"status"`
	LastAt    int64  `json:"lastAt,omitempty"`
	Message   string `json:"message,omitempty"`
	RetryEnd  bool   `json:"retryEnd,omitempty"`
}

type checkinDocument struct {
	Schema        int                       `json:"schema"`
	Date          string                    `json:"date"`
	Plans         map[string]*checkinRecord `json:"plans"`
	LastStartupAt int64                     `json:"lastStartupAt,omitempty"`
}

type checkinTarget struct {
	key       string
	account   *Account
	supported bool
	eligible  bool
}

type checkinJob struct {
	key     string
	date    string
	attempt int
	end     time.Time
	account *Account
	trace   string
	started time.Time
}

type checkinExpectedIdentity struct{}

type dailyCheckinScheduler struct {
	mu       sync.Mutex
	file     string
	loaded   bool
	blocked  bool
	doc      checkinDocument
	active   map[string]bool
	lastNext int64
	now      func() time.Time
	random   func(time.Duration) (time.Duration, error)
	perform  func(context.Context, *Account, string) (string, error)
}

func newDailyCheckinScheduler(file string) *dailyCheckinScheduler {
	return &dailyCheckinScheduler{file: file, now: time.Now, random: randomCheckinOffset,
		perform: checkinAccountWithTrace, active: map[string]bool{}}
}

func randomCheckinOffset(window time.Duration) (time.Duration, error) {
	// Whole-second targets match the scheduler's one-second polling cadence;
	// never choose the final fractional millisecond immediately before cutoff.
	n, err := rand.Int(rand.Reader, big.NewInt(int64(window/time.Second)))
	if err != nil {
		return 0, err
	}
	return time.Duration(n.Int64()) * time.Second, nil
}

func checkinWindows(now time.Time) (time.Time, time.Time, time.Time) {
	n := now.In(checkinZone)
	start := time.Date(n.Year(), n.Month(), n.Day(), 8, 0, 0, 0, checkinZone)
	return start, start.Add(90 * time.Minute), start.Add(2 * time.Hour)
}

func checkinDay(now time.Time) string { return now.In(checkinZone).Format("2006-01-02") }

// Caller holds accountMu. Stable across credential refresh/rename when UID exists.
func checkinTargetLocked(acc *Account) checkinTarget {
	if acc == nil {
		return checkinTarget{}
	}
	uid, enterprise := acc.UID, ""
	if acc.Auth != nil {
		uid, enterprise = acc.Auth.Account.UID, acc.Auth.Account.EnterpriseID
	}
	identity := "uid:" + uid
	if uid == "" {
		absolute, _ := filepath.Abs(acc.Path)
		identity = "file:" + filepath.Clean(absolute)
	}
	digest := sha256.Sum256([]byte(acc.Profile().Key + ":" + identity))
	supported := acc.Profile().Key == profileCN.Key && strings.TrimSpace(enterprise) == ""
	return checkinTarget{key: hex.EncodeToString(digest[:]), account: acc, supported: supported,
		eligible: supported && !acc.Disabled && acc.Auth != nil && acc.Auth.Auth.AccessToken != ""}
}

func currentCheckinTargets() []checkinTarget {
	accountMu.Lock()
	defer accountMu.Unlock()
	result := make([]checkinTarget, 0, len(accounts))
	for _, acc := range accounts {
		result = append(result, checkinTargetLocked(acc))
	}
	return result
}

func (s *dailyCheckinScheduler) loadLocked() {
	if s.loaded {
		return
	}
	s.loaded = true
	s.doc = checkinDocument{Schema: 1, Plans: map[string]*checkinRecord{}}
	info, err := os.Lstat(s.file)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8<<20 {
		s.blockLocked("State file unreadable or not a supported regular file")
		return
	}
	raw, err := os.ReadFile(s.file)
	var doc checkinDocument
	if err != nil || json.Unmarshal(raw, &doc) != nil || doc.Schema != 1 || doc.Plans == nil {
		s.blockLocked("State file corrupted; original file preserved")
		return
	}
	day, err := time.ParseInLocation("2006-01-02", doc.Date, checkinZone)
	if err != nil {
		s.blockLocked("State file date invalid; original file preserved")
		return
	}
	start, retry, end := checkinWindows(day)
	dayEnd := start.Add(16 * time.Hour)
	if doc.LastStartupAt != 0 && (doc.LastStartupAt < start.UnixMilli() || doc.LastStartupAt >= dayEnd.UnixMilli()) {
		s.blockLocked("Startup catch-up interval record invalid; original file preserved")
		return
	}
	for key, p := range doc.Plans {
		_, keyErr := hex.DecodeString(key)
		validState := p != nil && (p.Status == "pending" || p.Status == "running" ||
			p.Status == "success" || p.Status == "failed" || p.Status == "missed")
		if keyErr != nil || len(key) != 64 || !validState || p.Attempts < 0 || p.Attempts > 2 ||
			p.FirstAt < start.UnixMilli() || p.FirstAt >= retry.UnixMilli() ||
			p.RetryAt < retry.UnixMilli() || p.RetryAt >= end.UnixMilli() ||
			(p.StartupAt != 0 && (p.StartupAt < start.UnixMilli() || p.StartupAt >= dayEnd.UnixMilli())) ||
			(p.Status == "success" && p.Attempts == 0) {
			s.blockLocked("Invalid state file record; checkin paused to prevent duplicate requests")
			return
		}
	}
	s.doc = doc
	log.Printf("[Checkin state] traceId=%s stage=read result=success date=%s records=%d note=state restored only; no checkin sent", newTraceID(), doc.Date, len(doc.Plans))
}

func (s *dailyCheckinScheduler) blockLocked(reason string) {
	s.blocked = true
	log.Printf("[Checkin state] traceId=%s stage=persistence guard result=checkin paused reason=%s note=this process no longer sends checkin requests; original state file not overwritten", newTraceID(), reason)
}

func (s *dailyCheckinScheduler) saveLocked(trace string) bool {
	if s.blocked {
		return false
	}
	if info, err := os.Lstat(s.file); err == nil && !info.Mode().IsRegular() || err != nil && !errors.Is(err, os.ErrNotExist) {
		s.blockLocked("Write target is not a regular file or cannot be checked")
		return false
	}
	raw, err := json.MarshalIndent(s.doc, "", "  ")
	if err != nil {
		s.blockLocked("Failed to serialize state")
		return false
	}
	f, err := os.CreateTemp(filepath.Dir(s.file), ".checkin-state-*.tmp")
	if err != nil {
		s.blockLocked("Cannot create state temp file")
		return false
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	err = f.Chmod(0600)
	if err == nil {
		_, err = f.Write(append(raw, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, s.file)
	}
	if err != nil {
		s.blockLocked("State could not be written durably; no new checkin sent")
		return false
	}
	log.Printf("[Checkin state] traceId=%s stage=atomic write date=%s records=%d result=success note=attempt count saved first; upstream request allowed afterward", trace, s.doc.Date, len(s.doc.Plans))
	return true
}

// Startup after 08:00 queues only pending, never-attempted accounts. The first
// runs immediately; subsequent requests are spaced at least one minute apart.
// Credential hot reloads do not create a startup queue.
func (s *dailyCheckinScheduler) planAndClaim(now time.Time, targets []checkinTarget, startup bool) []checkinJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	if s.blocked {
		return nil
	}
	day := checkinDay(now)
	if s.doc.Date > day {
		s.blockLocked("Beijing time earlier than saved date; waiting for clock synchronization and restart")
		return nil
	}
	dirty := false
	if s.doc.Date != day {
		s.doc = checkinDocument{Schema: 1, Date: day, Plans: map[string]*checkinRecord{}}
		dirty = true
	}
	start, retry, end := checkinWindows(now)
	dayEnd := start.Add(16 * time.Hour)
	if startup {
		// Recover every interrupted record, including accounts removed from the
		// current pool, so an old in-flight marker cannot stall the whole queue.
		for _, p := range s.doc.Plans {
			if p.Status != "running" {
				continue
			}
			p.Status, p.Message = "failed", "Previous attempt result unconfirmed; original request not re-sent"
			if p.StartupAt != 0 && !now.Before(start) {
				s.doc.LastStartupAt = max(s.doc.LastStartupAt, now.UnixMilli())
			}
			if p.Attempts == 2 {
				p.RetryEnd = true
				p.Message += "; both attempts used today"
			}
			dirty = true
		}
	}
	s.active = map[string]bool{}
	var jobs []checkinJob
	var eligible []checkinTarget
	for _, target := range targets {
		if !target.eligible || s.active[target.key] {
			continue
		}
		s.active[target.key] = true
		eligible = append(eligible, target)
		p := s.doc.Plans[target.key]
		if p == nil {
			firstOffset, e1 := s.random(retry.Sub(start))
			retryOffset, e2 := s.random(end.Sub(retry))
			if e1 != nil || e2 != nil || firstOffset < 0 || firstOffset >= retry.Sub(start) ||
				retryOffset < 0 || retryOffset >= end.Sub(retry) {
				s.blockLocked("Cannot generate a valid random schedule")
				return nil
			}
			p = &checkinRecord{FirstAt: start.Add(firstOffset).UnixMilli(), RetryAt: retry.Add(retryOffset).UnixMilli(), Status: "pending"}
			s.doc.Plans[target.key] = p
			log.Printf("[Checkin plan] traceId=%s stage=generate date=%s account=%s first=%s retry=%s note=Beijing time; later restarts do not re-randomize",
				newTraceID(), day, target.key[:12], time.UnixMilli(p.FirstAt).In(checkinZone).Format("15:04:05"),
				time.UnixMilli(p.RetryAt).In(checkinZone).Format("15:04:05"))
			dirty = true
			if p.FirstAt <= now.UnixMilli() && !(startup && !now.Before(start)) {
				p.Status, p.Message = "missed", "Today's random checkin time has passed; account events do not trigger catch-up"
			}
		}
		if p.Status == "failed" && p.Attempts == 1 && !p.RetryEnd &&
			(!now.Before(end) || startup && p.RetryAt <= now.UnixMilli() ||
				p.StartupAt != 0 && max(p.RetryAt, s.startupDueLocked(p)) >= end.UnixMilli()) {
			p.RetryEnd, dirty = true, true
			p.Message = "Today's retry window has passed; no catch-up and no more retries today"
		}
		if p.Status == "pending" && p.StartupAt == 0 && !now.Before(retry) && !startup {
			p.Status, p.Message, dirty = "missed", "Today's first checkin window has passed; no catch-up", true
		}
		if p.Status == "pending" && p.StartupAt != 0 && s.startupDueLocked(p) >= dayEnd.UnixMilli() {
			p.Status, p.Message, dirty = "missed", "Not enough time left today to keep the one-minute spacing; no next-day catch-up", true
		}
	}
	if startup && !now.Before(start) {
		if s.queueStartupLocked(now, eligible, dayEnd) {
			dirty = true
		}
	}
	startupBusy := s.startupBusyLocked()
	for _, target := range eligible {
		p := s.doc.Plans[target.key]
		if p.Status == "success" || p.Status == "missed" || p.Status == "running" {
			continue
		}
		if p.StartupAt != 0 && startupBusy {
			continue
		}
		attempt, until := 0, retry
		if p.Status == "pending" && p.Attempts == 0 && p.StartupAt != 0 &&
			!now.Before(start) && now.Before(dayEnd) && s.startupDueLocked(p) <= now.UnixMilli() {
			attempt, until = 1, dayEnd
		} else if !startup && p.Status == "pending" && p.Attempts == 0 && p.StartupAt == 0 &&
			!now.Before(start) && now.Before(retry) && p.FirstAt <= now.UnixMilli() {
			attempt = 1
		} else if !startup && p.Status == "failed" && p.Attempts == 1 && !p.RetryEnd &&
			!now.Before(retry) && now.Before(end) && p.RetryAt <= now.UnixMilli() &&
			(p.StartupAt == 0 || s.startupDueLocked(p) <= now.UnixMilli()) {
			attempt, until = 2, end
		}
		if attempt == 0 {
			continue
		}
		p.Attempts, p.Status, p.LastAt = attempt, "running", now.UnixMilli()
		if p.StartupAt != 0 {
			s.doc.LastStartupAt = now.UnixMilli()
			startupBusy = true
		}
		p.Message = "Checking in according to the Beijing-time schedule"
		trace := newTraceID()
		jobs = append(jobs, checkinJob{key: target.key, date: day, attempt: attempt, end: until, account: target.account, trace: trace, started: now})
		dirty = true
	}
	trace := newTraceID()
	if dirty && !s.saveLocked(trace) {
		return nil
	}
	for _, job := range jobs {
		log.Printf("[Daily checkin] traceId=%s stage=attempt reserved and persisted date=%s account=%s attempt=%d deadline=%s result=allowed note=at most two per day; startup catch-up spaced at least one minute",
			job.trace, job.date, job.key[:12], job.attempt, job.end.In(checkinZone).Format("15:04:05"))
	}
	next := s.nextLocked(now)
	nextMillis := int64(0)
	if !next.IsZero() {
		nextMillis = next.UnixMilli()
	}
	if nextMillis != s.lastNext || startup || dirty {
		s.lastNext = nextMillis
		logScheduledTask("daily_checkin", "scheduled", "", trace, time.Time{}, next,
			"random 08:00–09:30 Beijing time; after 08:00 pending accounts are caught up every minute; failed retries run 09:30–10:00 once only; account events do not trigger catch-up")
	}
	return jobs
}

func (s *dailyCheckinScheduler) startupDueLocked(p *checkinRecord) int64 {
	return max(p.StartupAt, s.doc.LastStartupAt+time.Minute.Milliseconds())
}

func (s *dailyCheckinScheduler) startupBusyLocked() bool {
	for _, p := range s.doc.Plans {
		if p.StartupAt != 0 && p.Status == "running" {
			return true
		}
	}
	return false
}

func (s *dailyCheckinScheduler) queueStartupLocked(now time.Time, targets []checkinTarget, dayEnd time.Time) bool {
	var pending []checkinTarget
	for _, t := range targets {
		p := s.doc.Plans[t.key]
		if p.Status == "pending" && p.Attempts == 0 {
			pending = append(pending, t)
		}
	}
	// Keep already-persisted queue order across repeated restarts.
	sort.SliceStable(pending, func(i, j int) bool {
		a, b := s.doc.Plans[pending[i].key].StartupAt, s.doc.Plans[pending[j].key].StartupAt
		if a == 0 {
			return false
		}
		return b == 0 || a < b
	})
	slot := max(now.UnixMilli(), s.doc.LastStartupAt+time.Minute.Milliseconds())
	dirty := false
	for _, t := range pending {
		p := s.doc.Plans[t.key]
		at := max(slot, p.StartupAt)
		if at >= dayEnd.UnixMilli() {
			p.Status, p.Message = "missed", "Startup catch-up queue already beyond today in Beijing time; no next-day catch-up"
			dirty = true
			continue
		}
		if p.StartupAt != at {
			p.StartupAt = at
			dirty = true
		}
		message := "Queued for startup catch-up; at least one minute after the previous request completes"
		if p.Message != message {
			p.Message = message
			dirty = true
		}
		slot = at + time.Minute.Milliseconds()
	}
	if len(pending) > 0 {
		log.Printf("[Checkin plan] traceId=%s stage=startup catch-up queueing date=%s pending accounts=%d interval seconds=60 note=only pending accounts with zero attempts; already-signed and failed accounts are not queued; plan persisted first",
			newTraceID(), s.doc.Date, len(pending))
	}
	return dirty
}

func (s *dailyCheckinScheduler) nextLocked(now time.Time) time.Time {
	var next time.Time
	start, _, retryEnd := checkinWindows(now)
	for key, p := range s.doc.Plans {
		if !s.active[key] {
			continue
		}
		at := int64(0)
		if p.Status == "pending" && p.Attempts == 0 {
			at = p.FirstAt
			if p.StartupAt != 0 {
				at = s.startupDueLocked(p)
				if at >= start.Add(16*time.Hour).UnixMilli() {
					continue
				}
			}
		} else if p.Status == "failed" && p.Attempts == 1 && !p.RetryEnd {
			at = p.RetryAt
			if p.StartupAt != 0 {
				at = max(at, s.startupDueLocked(p))
			}
			if at >= retryEnd.UnixMilli() {
				continue
			}
		}
		if at > now.UnixMilli() && (next.IsZero() || time.UnixMilli(at).Before(next)) {
			next = time.UnixMilli(at)
		}
	}
	return next
}

func nextDailyCheckin(now time.Time) time.Time {
	dailyCheckins.mu.Lock()
	defer dailyCheckins.mu.Unlock()
	if next := dailyCheckins.nextLocked(now); !next.IsZero() {
		return next
	}
	start, _, _ := checkinWindows(now)
	if now.Before(start) {
		return start
	}
	start, _, _ = checkinWindows(now.In(checkinZone).AddDate(0, 0, 1))
	return start
}

func (s *dailyCheckinScheduler) runJob(ctx context.Context, job checkinJob) {
	logScheduledTask("daily_checkin", "started", "running", job.trace, job.started, time.Time{},
		fmt.Sprintf("Beijing-time date=%s attempt #%d; account=%s", job.date, job.attempt, job.key[:12]))
	// Derive the remaining budget from the same clock used for the schedule.
	// This also keeps deterministic virtual-clock tests independent of wall time.
	ctx, cancel := context.WithTimeout(ctx, job.end.Sub(s.now()))
	defer cancel()
	ctx = context.WithValue(ctx, checkinExpectedIdentity{}, job.key)
	result, err := "failed", error(context.DeadlineExceeded)
	if s.now().Before(job.end) && ctx.Err() == nil {
		result, err = s.perform(ctx, job.account, job.trace)
	}
	now := s.now()
	s.mu.Lock()
	p := s.doc.Plans[job.key]
	if s.doc.Date != job.date || p == nil || p.Attempts != job.attempt || p.Status != "running" {
		s.mu.Unlock()
		return
	}
	status := "failed"
	if p.StartupAt != 0 && checkinDay(now) == job.date {
		// Persist completion too: slow credential reads/network IO must not
		// collapse the one-minute gap between actual requests.
		s.doc.LastStartupAt = max(s.doc.LastStartupAt, now.UnixMilli())
	}
	if err == nil && (result == "ok" || result == "already") {
		p.Status, p.Message, status = "success", "Already checked in today (Beijing time)", "success"
	} else {
		p.Status, p.Message = "failed", safeCheckinFailure(err)
		if job.attempt == 2 {
			p.RetryEnd = true
			p.Message += "; both attempts used today, no more retries"
		} else {
			_, _, retryEnd := checkinWindows(now)
			if !now.Before(retryEnd) {
				p.RetryEnd = true
				p.Message += "; past today's retry window, no more retries"
			}
		}
	}
	s.saveLocked(job.trace)
	next := s.nextLocked(now)
	s.mu.Unlock()
	logScheduledTask("daily_checkin", "completed", status, job.trace, job.started, next,
		fmt.Sprintf("date=%s attempt=%d account=%s result=%s", job.date, job.attempt, job.key[:12], status))
	if status == "success" {
		requestQuotaScan()
	}
	if err := writeStatusSnapshot(); err != nil {
		log.Printf("[Daily checkin] traceId=%s stage=display snapshot result=failed note=checkin attempts already saved; next status snapshot cycle retries the display without re-sending checkin", job.trace)
	}
}

func backgroundDailyCheckin(ctx context.Context) {
	dailyCheckins.run(ctx)
}

func (s *dailyCheckinScheduler) run(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	var workers sync.WaitGroup
	defer workers.Wait()
	dispatch := func(jobs []checkinJob) {
		for _, job := range jobs {
			workers.Add(1)
			go func() {
				defer workers.Done()
				s.runJob(ctx, job)
			}()
		}
	}
	dispatch(s.planAndClaim(s.now(), currentCheckinTargets(), true))
	log.Printf("[Daily checkin] traceId=%s stage=startup result=plan restored per Beijing time note=before 08:00 only waiting on the random schedule; after 08:00 pending accounts start one-minute catch-up immediately; account events do not trigger catch-up", newTraceID())
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				return
			}
			dispatch(s.planAndClaim(s.now(), currentCheckinTargets(), false))
		}
	}
}

func (s *dailyCheckinScheduler) view(target checkinTarget, now time.Time) checkinStatusSnapshot {
	out := checkinStatusSnapshot{Date: checkinDay(now), Status: "pending"}
	if !target.supported {
		out.Status, out.Message = "unsupported", "International or enterprise accounts do not participate in personal checkin"
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	if s.blocked {
		out.Status, out.Message = "unknown", "Checkin state file unavailable; checkin paused to prevent duplicate requests"
		return out
	}
	if s.doc.Date == out.Date {
		if p := s.doc.Plans[target.key]; p != nil {
			out.Status, out.Attempts, out.LastAt, out.Message = p.Status, p.Attempts, p.LastAt/1000, p.Message
			if p.Status == "pending" && p.Attempts == 0 {
				out.NextAt = p.FirstAt / 1000
				if p.StartupAt != 0 {
					out.NextAt = s.startupDueLocked(p) / 1000
					start, _, _ := checkinWindows(now)
					if out.NextAt >= start.Add(16*time.Hour).Unix() {
						out.Status, out.NextAt = "missed", 0
						out.Message = "Not enough time left today to keep the one-minute spacing; no next-day catch-up"
					}
				}
			} else if p.Status == "failed" && p.Attempts == 1 && !p.RetryEnd {
				out.NextAt = p.RetryAt / 1000
				if p.StartupAt != 0 {
					out.NextAt = max(p.RetryAt, s.startupDueLocked(p)) / 1000
				}
				_, _, retryEnd := checkinWindows(now)
				if out.NextAt >= retryEnd.Unix() {
					out.NextAt = 0
				}
			}
			return out
		}
	}
	_, retry, _ := checkinWindows(now)
	if !now.Before(retry) {
		out.Status, out.Message = "missed", "Today's window has passed and no checkin was sent; no catch-up"
	}
	if !target.eligible {
		out.Message = "Account currently unavailable; no checkin sent"
	}
	return out
}

func checkinDisplay(view checkinStatusSnapshot, edition string, now time.Time) string {
	if profileForEdition(edition).Key != profileCN.Key || view.Status == "unsupported" {
		return "—"
	}
	if view.Date != checkinDay(now) {
		_, retry, _ := checkinWindows(now)
		if !now.Before(retry) {
			return "Not signed"
		}
		return "Pending"
	}
	switch view.Status {
	case "success":
		return "Signed"
	case "failed":
		return "Failed"
	case "missed":
		return "Not signed"
	case "unknown":
		return "Unknown"
	default:
		return "Pending"
	}
}

type checkinRequestError struct{ status, code int }

func (e checkinRequestError) Error() string {
	return fmt.Sprintf("Checkin failed (HTTP %d, business code %d)", e.status, e.code)
}

func safeCheckinFailure(err error) string {
	var known checkinRequestError
	if errors.As(err, &known) {
		return known.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "Checkin request timed out or exceeded its execution window"
	}
	if errors.Is(err, context.Canceled) {
		return "Checkin request was canceled"
	}
	return "Checkin request failed; see the log for the corresponding TraceID"
}

func checkinAccountWithTrace(ctx context.Context, acc *Account, trace string) (string, error) {
	log.Printf("[Daily checkin] traceId=%s stage=request entry note=uses only the saved random schedule; does not simulate completing activities", trace)
	if acc == nil {
		return "skipped", nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !tryAccountLockWithContext(ctx, acc.lock.TryRLock) {
		return "failed", ctx.Err()
	}
	accountMu.Lock()
	target := checkinTargetLocked(acc)
	var auth StoredAuth
	prof := *acc.Profile()
	if acc.Auth != nil {
		auth = *acc.Auth
	}
	accountMu.Unlock()
	acc.lock.RUnlock() // network IO never holds the model credential lock
	if expected, ok := ctx.Value(checkinExpectedIdentity{}).(string); ok && expected != target.key {
		log.Printf("[Daily checkin] traceId=%s stage=credential snapshot check result=skipped reason=account identity changed after scheduling note=old account attempts are never applied to a new account", trace)
		return "skipped", nil
	}
	if !target.eligible {
		result := "skipped"
		if prof.Key != profileCN.Key {
			result = "global_skipped"
		} else if auth.Account.EnterpriseID != "" {
			result = "enterprise_skipped"
		}
		log.Printf("[Daily checkin] traceId=%s stage=eligibility check result=skipped type=%s note=nothing sent upstream; schedule state unchanged", trace, result)
		return result, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, prof.dailyCheckinURL(), strings.NewReader("{}"))
	if err != nil {
		return "failed", err
	}
	commonHeaders(req, &prof)
	req.Header.Set("Authorization", "Bearer "+auth.Auth.AccessToken)
	req.Header.Set("X-Client-Platform", "web")
	if auth.Account.UID != "" {
		req.Header.Set("X-User-Id", auth.Account.UID)
	}
	if auth.Auth.Domain != "" {
		req.Header.Set("X-Domain", auth.Auth.Domain)
	}
	client := *cfg.HttpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	log.Printf("[Daily checkin] traceId=%s stage=upstream request method=POST account=%s note=sent once; no automatic network retry; credentials and raw responses never logged", trace, target.key[:12])
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[Daily checkin] traceId=%s stage=request error type=%s result=failed note=original account state kept; whether to retry is decided solely by today's plan", trace, debugErrorType(err))
		return "failed", err
	}
	defer resp.Body.Close()
	var envelope struct {
		Code *int   `json:"code"`
		Msg  string `json:"msg"`
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	parseErr := json.Unmarshal(raw, &envelope)
	code := -1
	if envelope.Code != nil {
		code = *envelope.Code
	}
	log.Printf("[Daily checkin] traceId=%s stage=response validation HTTP=%d business code=%d note=only explicit success or already-checked-in accepted; error responses never counted as success", trace, resp.StatusCode, code)
	if readErr == nil && len(raw) <= 1<<20 && parseErr == nil {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 && code == 0 {
			return "ok", nil
		}
		if (resp.StatusCode >= 200 && resp.StatusCode < 300 || resp.StatusCode == 400 || resp.StatusCode == 409) &&
			(code == 10001 || code == 14001 || code == -1 && isAlreadyCheckedIn(resp.StatusCode, envelope.Msg)) {
			return "already", nil
		}
	}
	return "failed", checkinRequestError{status: resp.StatusCode, code: code}
}
