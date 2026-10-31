// Package provisioning implements the durable, DB-backed job runner in the
// panel. Job rows and deployment events persist in the panel store
// (SQLite locally, MariaDB in production); secrets for a
// create job (the WordPress admin password) live only in process memory and
// are lost on restart - the job is then marked interrupted on boot by the
// store's crash recovery and the operator can retry creation.
package provisioning

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Udhayaboopathi/hPanel-wp/internal/agentclient"
	"github.com/Udhayaboopathi/hPanel-wp/internal/agentproto"
	"github.com/Udhayaboopathi/hPanel-wp/internal/jobs"
	"github.com/Udhayaboopathi/hPanel-wp/internal/store"
)

type Runner struct {
	Store *store.Store
	Agent *agentclient.Client
	Sem   chan struct{} // bounds concurrent provisioning jobs to 2

	mu      sync.Mutex
	secrets map[string]string // jobID -> wp admin password (memory only)
	cancels map[string]context.CancelFunc
}

func New(st *store.Store, agent *agentclient.Client) *Runner {
	return &Runner{Store: st, Agent: agent, Sem: make(chan struct{}, 2), secrets: map[string]string{}, cancels: map[string]context.CancelFunc{}}
}

type CreatePayload struct {
	Spec agentproto.SiteSpec `json:"spec"`
}

func (r *Runner) Enqueue(site *store.Website, kind string, spec agentproto.SiteSpec, adminPassword string) (*store.Job, error) {
	if err := jobs.ValidateKind(kind); err != nil {
		return nil, err
	}
	if kind == "create_site" {
		// One active job per site across ALL kinds: a create must
		// never race a delete/backup/lifecycle job for the same site.
		active, err := r.Store.HasActiveJobForSite(site.SiteID, "")
		if err != nil {
			return nil, err
		}
		if active {
			return nil, fmt.Errorf("another job is already active for site %s; wait for it to finish first", site.SiteID)
		}
	}
	// Payload stored in the DB never contains passwords.
	specCopy := spec
	specCopy.DBPassword = ""
	specCopy.AdminPassword = ""
	payload, _ := json.Marshal(CreatePayload{Spec: specCopy})
	job := &store.Job{
		WebsiteID:   sql.NullInt64{Int64: site.ID, Valid: site.ID != 0},
		SiteID:      site.SiteID,
		Kind:        kind,
		Status:      "queued",
		PayloadJSON: string(payload),
	}
	if err := r.Store.CreateJob(job); err != nil {
		return nil, err
	}
	if adminPassword != "" {
		r.mu.Lock()
		r.secrets[job.ID] = adminPassword
		r.mu.Unlock()
	}
	_ = r.Store.AddEvent(job.ID, "info", "job queued: "+kind)
	go r.run(job.ID, spec, adminPassword)
	return job, nil
}

func (r *Runner) EnqueueSimple(site *store.Website, kind string, req agentproto.Request) (*store.Job, error) {
	job := &store.Job{
		WebsiteID: sql.NullInt64{Int64: site.ID, Valid: site.ID != 0},
		SiteID:    site.SiteID, Kind: kind, Status: "queued", PayloadJSON: "{}",
	}
	if err := jobs.ValidateKind(kind); err != nil {
		return nil, err
	}
	// One active job per site across all kinds (double-clicks and
	// cross-page submissions included): parallel jobs against one
	// site's container/database are how resources get corrupted.
	if active, err := r.Store.HasActiveJobForSite(site.SiteID, ""); err != nil {
		return nil, err
	} else if active {
		return nil, fmt.Errorf("another job is already active for site %s; wait for it to finish first", site.SiteID)
	}
	if err := r.Store.CreateJob(job); err != nil {
		return nil, err
	}
	_ = r.Store.AddEvent(job.ID, "info", "job queued: "+kind)
	go func() {
		req.JobID = job.ID
		r.execute(job.ID, req, kind)
	}()
	return job, nil
}

func (r *Runner) run(jobID string, spec agentproto.SiteSpec, adminPassword string) {
	spec.AdminPassword = adminPassword
	op := agentproto.OpCreateSite
	req := agentproto.Request{Op: op, Site: spec, JobID: jobID}
	r.execute(jobID, req, "create_site")
	r.mu.Lock()
	delete(r.secrets, jobID)
	r.mu.Unlock()
}

func (r *Runner) execute(jobID string, req agentproto.Request, kind string) {
	r.Sem <- struct{}{}
	defer func() { <-r.Sem }()
	job, err := r.Store.GetJob(jobID)
	if err != nil {
		return
	}
	if err := jobs.ValidateTransition(job.Status, "running"); err != nil && job.Status != "queued" {
		_ = r.Store.AddEvent(jobID, "error", "cannot start job: "+err.Error())
		return
	}
	_ = r.Store.BumpJobAttempts(jobID)
	job.Attempts++ // BumpJobAttempts just incremented it in the store
	_ = r.Store.UpdateJob(jobID, "running", 5, "provisioning started", "")
	_ = r.Store.AddEvent(jobID, "info", "provisioning started")
	// lastProgress tracks the furthest step actually reached, so a
	// FAILED job never displays 100% (which must mean success only).
	lastProgress := 5
	if kind == "create_site" {
		_ = r.Store.UpdateWebsite(req.Site.SiteID, map[string]any{"status": "provisioning"})
	}
	// fail marks the job failed at the progress it actually reached,
	// or schedules a bounded retry when the failure is transient
	// (transport-level: agent/daemon unreachable) and attempts remain.
	// Operational failures reported BY the agent (missing container,
	// guard refusals, bad config) are never auto-retried: repeating
	// them cannot succeed and could compound side effects.
	fail := func(msg string, retryable bool) {
		if retryable && job.MaxAttempts > 0 && job.Attempts < job.MaxAttempts {
			backoff := time.Duration(job.Attempts*job.Attempts) * 10 * time.Second
			_ = r.Store.AddEvent(jobID, "warn", fmt.Sprintf("attempt %d failed (transient): %s - retry %d/%d scheduled in %s", job.Attempts, msg, job.Attempts+1, job.MaxAttempts, backoff))
			_ = r.Store.UpdateJob(jobID, "queued", lastProgress, "retry scheduled", "")
			time.AfterFunc(backoff, func() { r.execute(jobID, req, kind) })
			return
		}
		_ = r.Store.UpdateJob(jobID, "failed", lastProgress, "failed", msg)
		_ = r.Store.AddEvent(jobID, "error", msg)
		if kind == "create_site" {
			_ = r.Store.UpdateWebsite(req.Site.SiteID, map[string]any{"status": "failed"})
		}
	}
	if r.Agent == nil || !r.Agent.Available() {
		fail("provisioning agent socket is not available; deploy the agent (see docs/DEPLOYMENT.md) - no resources were created", true)
		return
	}
	// The call context is registered so Cancel() can actually abort
	// an in-flight job (previously Cancel only changed the row and
	// the running goroutine overwrote it with its own outcome).
	callCtx, stopCall := context.WithCancel(context.Background())
	r.mu.Lock()
	r.cancels[jobID] = stopCall
	r.mu.Unlock()
	defer func() {
		stopCall()
		r.mu.Lock()
		delete(r.cancels, jobID)
		r.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(callCtx, 12*time.Minute)
	defer cancel()
	resp, err := r.Agent.Call(ctx, req)
	if cur, gerr := r.Store.GetJob(jobID); gerr == nil && cur.Status == "cancelled" {
		// Cancelled while the agent call was in flight: the row keeps
		// its cancelled state; the late result is discarded. (A Docker
		// action the agent had already sent may still have completed -
		// the event says so honestly.)
		_ = r.Store.AddEvent(jobID, "warn", "job cancelled by operator; the agent result was discarded (an already-sent Docker action may still have taken effect)")
		return
	}
	if err != nil {
		fail(err.Error(), true) // transport failure: agent/daemon hiccup, retryable
		return
	}
	for _, ev := range resp.Events {
		_ = r.Store.AddEvent(jobID, ev.Level, ev.Message)
		if ev.Progress > lastProgress {
			lastProgress = ev.Progress
		}
		_ = r.Store.UpdateJob(jobID, "running", ev.Progress, ev.Message, "")
	}
	if resp.UsedCommonDBUser {
		// The agent fell back to the common DB user for this site
		// (admin account lacks CREATE USER/GRANT). This must be visible in
		// the audit log, not only in job events - never fail silently.
		_ = r.Store.AddAudit(0, "db.common_user_fallback", req.Site.SiteID,
			"site provisioned with the common DB user because the DB admin account could not create a dedicated user/grant; per-site database isolation is reduced for this site", "")
	}
	if !resp.OK {
		// A missing container is a precise, recoverable state - not a
		// generic failure. Record it on the website so the UI can offer
		// Repair instead of a blind retry.
		if resp.ErrorCategory == agentproto.ErrCatNotFound {
			switch kind {
			case "start_site", "stop_site", "restart_site":
				_ = r.Store.UpdateWebsite(req.Site.SiteID, map[string]any{"status": "missing"})
			}
		}
		// Agent-reported operational failures are not auto-retried.
		fail(resp.Error, false)
		return
	}
	_ = r.Store.UpdateJob(jobID, "succeeded", 100, "completed", "")
	_ = r.Store.AddEvent(jobID, "info", "completed")
	if kind == "backup_site" && req.BackupID > 0 && resp.BackupPath != "" {
		// Record the agent's authoritative artifact facts (exact
		// path + checksum + size) on the backup record. The panel's
		// reconciler keeps a directory-glob fallback for jobs whose
		// response carried no path.
		_ = r.Store.CompleteBackup(req.BackupID, resp.BackupPath[strings.LastIndex(resp.BackupPath, "/")+1:], resp.BackupPath, resp.BackupSHA256, resp.BackupSize)
	}
	switch kind {
	case "create_site", "repair_site":
		status := "ready"
		if resp.DNSPending {
			status = "dns_pending"
		}
		fields := map[string]any{"status": status}
		if resp.ContainerID != "" {
			fields["container_id"] = resp.ContainerID
		}
		_ = r.Store.UpdateWebsite(req.Site.SiteID, fields)
	case "stop_site":
		fields := map[string]any{"status": "stopped"}
		if resp.ContainerID != "" {
			fields["container_id"] = resp.ContainerID
		}
		_ = r.Store.UpdateWebsite(req.Site.SiteID, fields)
	case "start_site", "restart_site":
		// Reconcile the stored container identity with reality on
		// every successful lifecycle operation: the agent resolved
		// the container by labels, so its ID is the current truth
		// even after a recreation changed it.
		fields := map[string]any{"status": "ready"}
		if resp.ContainerID != "" {
			fields["container_id"] = resp.ContainerID
		}
		_ = r.Store.UpdateWebsite(req.Site.SiteID, fields)
	}
}

// Cancel marks a queued/running job cancelled AND aborts its
// in-flight agent call when one is running (the executor notices the
// cancelled row when the call unwinds and discards the late result
// instead of overwriting the cancellation). Cancelling a job whose
// retry timer is pending works too: the timer's re-entry sees the
// cancelled state and stops.
func (r *Runner) Cancel(jobID string) error {
	job, err := r.Store.GetJob(jobID)
	if err != nil {
		return err
	}
	if err := jobs.ValidateTransition(job.Status, "cancelled"); err != nil {
		return err
	}
	_ = r.Store.AddEvent(jobID, "warn", "cancellation requested by operator")
	if err := r.Store.UpdateJob(jobID, "cancelled", job.Progress, "cancelled by operator", ""); err != nil {
		return err
	}
	r.mu.Lock()
	stop := r.cancels[jobID]
	r.mu.Unlock()
	if stop != nil {
		stop()
	}
	return nil
}

// Retry is retained for API compatibility but no longer pretends to
// re-queue: marking a job "queued" without an executor behind it was
// a real defect (the job sat queued forever). Actual re-execution
// lives in the panel's job-retry handler, which enqueues a FRESH job
// for kinds whose request can be reconstructed from the website
// record, and explains itself for the rest. This method only reports
// that distinction.
func (r *Runner) Retry(jobID string) error {
	job, err := r.Store.GetJob(jobID)
	if err != nil {
		return err
	}
	if job.Status != "failed" && job.Status != "interrupted" && job.Status != "cancelled" {
		return fmt.Errorf("only failed, interrupted or cancelled jobs can be retried")
	}
	return fmt.Errorf("retry re-executes as a new job from the job page; it is not a state change on this job")
}
