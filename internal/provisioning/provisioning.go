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
}

func New(st *store.Store, agent *agentclient.Client) *Runner {
	return &Runner{Store: st, Agent: agent, Sem: make(chan struct{}, 2), secrets: map[string]string{}}
}

type CreatePayload struct {
	Spec agentproto.SiteSpec `json:"spec"`
}

func (r *Runner) Enqueue(site *store.Website, kind string, spec agentproto.SiteSpec, adminPassword string) (*store.Job, error) {
	if err := jobs.ValidateKind(kind); err != nil {
		return nil, err
	}
	if kind == "create_site" {
		active, err := r.Store.HasActiveJobForSite(site.SiteID, kind)
		if err != nil {
			return nil, err
		}
		if active {
			return nil, fmt.Errorf("a creation job is already active for site %s", site.SiteID)
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
	_ = r.Store.UpdateJob(jobID, "running", 5, "provisioning started", "")
	_ = r.Store.AddEvent(jobID, "info", "provisioning started")
	if kind == "create_site" {
		_ = r.Store.UpdateWebsite(req.Site.SiteID, map[string]any{"status": "provisioning"})
	}
	if r.Agent == nil || !r.Agent.Available() {
		msg := "provisioning agent socket is not available; deploy the agent (see docs/DEPLOYMENT.md) - no resources were created"
		_ = r.Store.UpdateJob(jobID, "failed", 0, "agent unavailable", msg)
		_ = r.Store.AddEvent(jobID, "error", msg)
		if kind == "create_site" {
			_ = r.Store.UpdateWebsite(req.Site.SiteID, map[string]any{"status": "failed"})
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	resp, err := r.Agent.Call(ctx, req)
	if err != nil {
		_ = r.Store.UpdateJob(jobID, "failed", 0, "agent call failed", err.Error())
		_ = r.Store.AddEvent(jobID, "error", err.Error())
		if kind == "create_site" {
			_ = r.Store.UpdateWebsite(req.Site.SiteID, map[string]any{"status": "failed"})
		}
		return
	}
	for _, ev := range resp.Events {
		_ = r.Store.AddEvent(jobID, ev.Level, ev.Message)
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
		_ = r.Store.UpdateJob(jobID, "failed", 100, "failed", resp.Error)
		_ = r.Store.AddEvent(jobID, "error", resp.Error)
		if kind == "create_site" {
			_ = r.Store.UpdateWebsite(req.Site.SiteID, map[string]any{"status": "failed"})
		}
		return
	}
	_ = r.Store.UpdateJob(jobID, "succeeded", 100, "completed", "")
	_ = r.Store.AddEvent(jobID, "info", "completed")
	switch kind {
	case "create_site":
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
		_ = r.Store.UpdateWebsite(req.Site.SiteID, map[string]any{"status": "stopped"})
	case "start_site", "restart_site":
		_ = r.Store.UpdateWebsite(req.Site.SiteID, map[string]any{"status": "ready"})
	}
}

// Cancel marks a queued/running job cancelled. Cancellation is cooperative:
// the agent checks the request context between provisioning steps.
func (r *Runner) Cancel(jobID string) error {
	job, err := r.Store.GetJob(jobID)
	if err != nil {
		return err
	}
	if err := jobs.ValidateTransition(job.Status, "cancelled"); err != nil {
		return err
	}
	_ = r.Store.AddEvent(jobID, "warn", "cancellation requested by operator")
	return r.Store.UpdateJob(jobID, "cancelled", job.Progress, "cancelled by operator", "")
}

// Retry re-queues a failed/interrupted job whose payload is still present.
// Create jobs need the admin password re-supplied because it is never stored.
func (r *Runner) Retry(jobID string) error {
	job, err := r.Store.GetJob(jobID)
	if err != nil {
		return err
	}
	if job.Status != "failed" && job.Status != "interrupted" && job.Status != "cancelled" {
		return fmt.Errorf("only failed, interrupted or cancelled jobs can be retried")
	}
	return r.Store.UpdateJob(jobID, "queued", 0, "queued for retry - re-run the operation from the website page", "")
}
