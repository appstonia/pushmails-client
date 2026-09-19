// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

// Package runner drives the claim → send → ack loop.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/appstonia/pushmails-client/internal/agent"
	"github.com/appstonia/pushmails-client/internal/config"
	"github.com/appstonia/pushmails-client/internal/mta"
	"github.com/appstonia/pushmails-client/internal/policy"
	"github.com/appstonia/pushmails-client/internal/preflight"
	"github.com/appstonia/pushmails-client/internal/sender"
)

// Transport puts one built message on the wire. Either mta.Direct, which talks
// to the recipient's server itself, or mta.Relay, which hands it to yours.
type Transport interface {
	Deliver(ctx context.Context, del mta.Delivery) mta.Outcome
	Close()
}

// Implemented by the relay only. Direct delivery has no relay to reach, so the
// health report leaves the answer out rather than inventing one — the server
// reads a missing value as "not measured", not as a failure.
type prober interface {
	Probe() error
}

// Implemented by direct delivery only: the pacing key comes from the primary
// MX, and idle sessions are swept between batches.
type mxTransport interface {
	PrimaryMX(ctx context.Context, domain string) (string, error)
	Sweep()
}

type Runner struct {
	cfg       *config.Config
	client    *agent.Client
	transport Transport
	keys      *sender.Keyring
	hostname  string

	// Set in direct mode only. Nothing in here applies to a relay: it has its
	// own idea of how fast to go and how long to back off, and second-guessing
	// it from out here would only fight it.
	mx       mxTransport
	pacer    *policy.Pacer
	throttle *policy.Throttle

	// Set when the server says this node cannot send. Written by the preflight
	// loop, read by the claim loop, so it is atomic.
	blocked atomic.Bool
	// Wakes the preflight loop early — after a blocked claim, or after a batch
	// that failed the way a blocklisting fails.
	recheck chan struct{}

	// Limits from `hello` and the measured send rate. The heartbeat refreshes
	// the limits from another goroutine, so they are behind the mutex.
	mu        sync.Mutex
	lease     time.Duration
	maxBatch  int
	rate      float64 // messages per second
	inventory []agent.IP

	// Port 25 probe state: the target and cadence come from the server, the
	// result is cached between preflights so we do not dial someone else's
	// mail server every few minutes.
	port25Target    string
	port25Interval  time.Duration
	port25Status    string
	port25Error     string
	port25CheckedAt time.Time
}

func New(cfg *config.Config, client *agent.Client, tr Transport) *Runner {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	r := &Runner{
		cfg:          cfg,
		client:       client,
		transport:    tr,
		keys:         sender.NewKeyring(client),
		hostname:     hostname,
		maxBatch:     cfg.BatchSize,
		lease:        5 * time.Minute,
		recheck:      make(chan struct{}, 1),
		port25Status: agent.Port25Skipped,
	}
	if mx, ok := tr.(mxTransport); ok {
		r.mx = mx
		r.pacer = policy.NewPacer()
		r.throttle = policy.NewThrottle()
	}
	return r
}

const (
	// Wait suggested for jobs released because the lease window closed. Without
	// it the job comes straight back and the next batch stalls in the same place.
	windowClosedRetryAfter = 30

	// Time reserved at the end of the lease for the final ack. Sending stops
	// then; a report that arrives after the lease is rejected and the job is
	// sent twice.
	ackReserve = 15 * time.Second

	// How often we say "still here". The server derives node status from the
	// last request, and claim/ack do not arrive on a schedule: a client sending
	// to a slow target makes no request for minutes and looks offline. The
	// heartbeat closes that gap — hello already refreshes the timestamp, so no
	// extra endpoint is needed.
	heartbeatInterval = 60 * time.Second

	// Smallest batch we will claim. Below this the round trip costs more than
	// the work it fetches.
	minBatch = 10

	// How long the send loop naps while blocked. Short, so sending resumes
	// promptly once preflight clears the flag.
	blockedPollInterval = 15 * time.Second

	// Wait asked for when a message cannot be signed. Long enough that a
	// domain waiting on DKIM verification in the panel is not re-fetched every
	// few seconds, short enough that finishing that verification starts
	// delivery the same minute.
	unsignableRetryAfter = 300
)

// Incremental ack: results are reported in chunks rather than at the end of the
// batch. Holding them means the first message's outcome sits here for minutes,
// and a crash in that window re-sends mail that was already delivered.
const (
	ackChunk    = 50
	ackInterval = 5 * time.Second
	// Most results the server takes in one request.
	ackMaxChunk = 500
	ackTimeout  = 30 * time.Second
)

// Bounds this client puts on the server's preflight interval. Not policy, only
// a guard: a bug on either side must not turn every installation into a
// one-second polling loop, nor let a node go a day without checking.
const (
	minPreflightInterval = 60 * time.Second
	maxPreflightInterval = time.Hour
	// Used when preflight itself fails, so an outage is retried without
	// hammering.
	preflightRetryInterval = 5 * time.Minute
)

// A batch this size, failing this often at the session stage, is what a fresh
// blocklisting looks like from here: the relay accepts nothing and every
// rejection lands before RCPT.
const (
	sessionFailureSample    = 5
	sessionFailureThreshold = 0.5
)

func (r *Runner) Run(ctx context.Context) error {
	if err := r.handshake(ctx); err != nil {
		return err
	}
	defer r.transport.Close()

	// Ask before sending anything. A failure here is not fatal: see runPreflight.
	interval := r.runPreflight(ctx)

	// Both loops are independent of the main one: sharing it would keep them
	// from firing exactly when they are needed, in the middle of a long batch.
	go r.heartbeat(ctx)
	go r.preflightLoop(ctx, interval)

	failures := 0
	for {
		if ctx.Err() != nil {
			slog.Info("shutdown signal received, loop stopped")
			return nil
		}

		// While blocked the loop keeps running but claims nothing. The
		// heartbeat and preflight continue, so the node stays visible and
		// learns the moment it is unblocked; a silent client looks like a
		// network fault and sends an operator hunting for nothing.
		if r.blocked.Load() {
			if !sleepCtx(ctx, blockedPollInterval) {
				return nil
			}
			continue
		}

		wait, err := r.tick(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			failures++
			backoff := backoffDuration(failures)
			slog.Error("round failed, will retry",
				"error", err, "attempt", failures, "backoff", backoff)
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			continue
		}

		failures = 0
		if wait > 0 && !sleepCtx(ctx, wait) {
			return nil
		}
	}
}

// A failure is logged, never fatal.
func (r *Runner) heartbeat(ctx context.Context) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hello, err := r.client.Hello(ctx, r.hostname)
			if err != nil {
				if ctx.Err() == nil {
					slog.Warn("heartbeat failed", "error", err)
				}
				continue
			}
			r.applyLimits(hello)
			r.applyInventory(hello.IPs)
		}
	}
}

// A rejected token will not start working on its own, so those errors end the
// process: the operator has to issue a new one. Anything temporary (the API
// restarting, a network blip) is retried, otherwise a one-minute outage would
// take every client down with it.
func (r *Runner) handshake(ctx context.Context) error {
	attempts := 0
	for {
		hello, err := r.client.Hello(ctx, r.hostname)
		if err == nil {
			r.applyHello(hello)
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}

		var apiErr *agent.APIError
		if errors.As(err, &apiErr) && !apiErr.Retryable() {
			// A collector token can only report feedback; the sending
			// endpoints are closed to it. Retrying fixes neither that nor an
			// invalid token.
			return err
		}

		attempts++
		backoff := backoffDuration(attempts)
		slog.Error("could not reach the central system, will retry",
			"error", err, "attempt", attempts, "backoff", backoff)
		if !sleepCtx(ctx, backoff) {
			return nil
		}
	}
}

func (r *Runner) applyHello(hello *agent.HelloResponse) {
	r.applyLimits(hello)
	r.client.SetPollWait(time.Duration(hello.PollWaitSeconds) * time.Second)

	if hello.Mode != "self_hosted" {
		// This token belongs to the central pool, which never hands work to a
		// customer's client: the queue would just look empty forever.
		slog.Warn("this token is not a self-hosted one; the queue will look empty",
			"mode", hello.Mode)
	}

	r.mu.Lock()
	lease, maxBatch := r.lease, r.maxBatch
	r.mu.Unlock()

	slog.Info("connected to the central system",
		"node", hello.Name,
		"mode", hello.Mode,
		"hourly_limit", hello.RateLimitPerHour,
		"batch", maxBatch,
		"lease", lease,
	)
	r.applyInventory(hello.IPs)
}

// The configured batch size is a ceiling too: the server's limit protects the
// queue, the flag is what the operator asked for, and neither may be exceeded.
func (r *Runner) applyLimits(hello *agent.HelloResponse) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if hello.LeaseSeconds > 0 {
		r.lease = time.Duration(hello.LeaseSeconds) * time.Second
	}
	maxBatch := r.cfg.BatchSize
	if hello.MaxBatchSize > 0 && hello.MaxBatchSize < maxBatch {
		maxBatch = hello.MaxBatchSize
	}
	if maxBatch != r.maxBatch {
		slog.Debug("batch ceiling updated", "from", r.maxBatch, "to", maxBatch)
	}
	r.maxBatch = maxBatch
}

// Informational only. Mail leaves through the relay in -smtp-host, and that
// relay picks the outgoing address, so this client neither selects nor binds
// one. Logging the list makes a mismatch visible: an address registered
// centrally that your MTA never sends from is a misconfiguration.
func (r *Runner) applyInventory(ips []agent.IP) {
	r.mu.Lock()
	previous := r.inventory
	r.inventory = ips
	r.mu.Unlock()

	listedBefore := map[string]bool{}
	for _, ip := range previous {
		listedBefore[ip.IP] = ip.Reputation == agent.ReputationListed
	}

	for _, ip := range ips {
		if ip.Reputation == agent.ReputationListed && !listedBefore[ip.IP] {
			// Loud, because the customer's own log is the only place this
			// shows up on their side. Not acted on: whether a listed address
			// stops sending is the server's decision.
			slog.Error("sending address is blocklisted",
				"ip", ip.IP, "zones", ip.ListedZones)
			continue
		}
		slog.Debug("sending address registered for this node",
			"ip", ip.IP, "ptr", ip.PTR, "dedicated", ip.Dedicated,
			"status", ip.Status, "reputation", ip.Reputation)
	}
}

// The returned duration is how long to wait before the next round.
func (r *Runner) tick(ctx context.Context) (time.Duration, error) {
	batchSize := r.nextBatchSize()

	claim, err := r.client.Claim(ctx, batchSize)
	if err != nil {
		return 0, err
	}

	// The server refuses to hand out work. This is the gate that cannot be
	// skipped: when preflight is unreachable the client keeps sending, so
	// claim has to carry the same verdict.
	if claim.Blocked {
		reason := ""
		if claim.BlockedReason != nil {
			reason = *claim.BlockedReason
		}
		if !r.blocked.Swap(true) {
			slog.Error("the server is not handing out work for this node", "reason", reason)
		}
		r.requestPreflight()
		return 0, nil
	}

	if len(claim.Jobs) == 0 {
		wait := time.Duration(claim.RetryAfterSeconds) * time.Second
		if wait <= 0 {
			// The server already held the long-poll open; a short breather
			// keeps us from asking again immediately for nothing.
			wait = 2 * time.Second
		}
		slog.Debug("queue empty", "remaining_quota", claim.RemainingQuota, "wait", wait)
		return wait, nil
	}

	slog.Info("jobs claimed",
		"count", len(claim.Jobs), "requested", batchSize,
		"remaining_quota", claim.RemainingQuota)

	started := time.Now()
	total := r.runBatch(ctx, claim.Jobs)
	elapsed := time.Since(started)
	r.observeRate(len(claim.Jobs), elapsed)

	// A run of session-stage rejections is the earliest sign of a blocklisting:
	// the relay refuses us before it ever hears the recipient. Ask now rather
	// than burning the rest of the interval on batches that cannot land.
	if total.total() >= sessionFailureSample &&
		float64(total.sessionFailures)/float64(total.total()) >= sessionFailureThreshold {
		slog.Warn("most of the batch was rejected before the recipient stage; "+
			"re-checking node health",
			"rejected", total.sessionFailures, "batch", total.total())
		r.requestPreflight()
	}

	slog.Info("batch finished",
		"sent", total.sent, "failed", total.failed, "bounced", total.bounced,
		"released", total.released, "accepted", total.accepted,
		"took", elapsed.Round(time.Millisecond))

	// Between batches, not during: a campaign touches thousands of domains and
	// neither the pooled connections nor the pacing keys should outlive their
	// usefulness in a process that stays up for weeks.
	if r.mx != nil {
		r.mx.Sweep()
		r.pacer.Sweep(time.Now())
	}

	// There may be more work waiting; take the next batch right away.
	return 0, nil
}

// A fixed large batch risks overrunning the lease, and an overrun batch is
// taken back by the server and sent twice. Aiming at half of what the measured
// rate fits in a lease leaves room for the rate to drop mid-batch (a slow
// target, greylisting).
func (r *Runner) nextBatchSize() int {
	r.mu.Lock()
	rate, lease, maxBatch := r.rate, r.lease, r.maxBatch
	r.mu.Unlock()

	if rate <= 0 || lease <= 0 {
		return maxBatch
	}
	return clamp(int(rate*lease.Seconds()*0.5), min(minBatch, maxBatch), maxBatch)
}

// Moving average, so one slow batch does not throw the estimate off.
func (r *Runner) observeRate(jobs int, elapsed time.Duration) {
	if jobs == 0 || elapsed <= 0 {
		return
	}
	observed := float64(jobs) / elapsed.Seconds()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rate == 0 {
		r.rate = observed
		return
	}
	r.rate = 0.7*r.rate + 0.3*observed
}

type counts struct {
	sent, failed, bounced, released int
	accepted                        int
	// Rejections received at the session stage — the shape a blocklisting takes.
	sessionFailures int
}

func (c counts) total() int { return c.sent + c.failed + c.bounced + c.released }

func (c *counts) add(result result) {
	switch result.Result.Status {
	case agent.StatusSent:
		c.sent++
	case agent.StatusBounced:
		c.bounced++
	case agent.StatusReleased:
		c.released++
	default:
		c.failed++
	}
	if result.sessionFailure {
		c.sessionFailures++
	}
}

// result is one outcome plus the local detail the server has no field for.
type result struct {
	agent.Result
	sessionFailure bool
}

func (r *Runner) runBatch(ctx context.Context, jobs []agent.Job) counts {
	r.mu.Lock()
	lease := r.lease
	r.mu.Unlock()

	// The batch has to finish inside the lease, with time left for the ack.
	sendCtx := ctx
	if lease > ackReserve {
		var cancel context.CancelFunc
		sendCtx, cancel = context.WithTimeout(ctx, lease-ackReserve)
		defer cancel()
	}

	results := make(chan result, len(jobs))

	// The ack must not depend on the sending context: even on a shutdown
	// signal we report the work we did before leaving.
	ackCtx := context.WithoutCancel(ctx)
	var ackWG sync.WaitGroup
	var total counts
	ackWG.Add(1)
	go func() {
		defer ackWG.Done()
		total = r.ackLoop(ackCtx, results)
	}()

	// Fetch whatever keys this batch needs before any of it goes out, in one
	// request rather than one per message. Only domains that are missing or
	// stale are asked for.
	r.keys.Ensure(sendCtx, fromAddresses(jobs))

	routed, released := r.route(jobs)
	for _, res := range released {
		results <- res
	}
	r.dispatch(sendCtx, routed, results)

	close(results)
	ackWG.Wait()
	return total
}

// fromAddresses lists the sender addresses in a batch. One batch can carry
// several, because a node sends for every domain its account has verified.
func fromAddresses(jobs []agent.Job) []string {
	out := make([]string, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, job.FromEmail)
	}
	return out
}

// routedJob is a job with its destination worked out.
type routedJob struct {
	job    agent.Job
	domain string
	// Most messages one session may carry. Zero in relay mode: the relay
	// decides how it talks to the world.
	maxPerConn int
}

// route works out where each job is going and groups the ones that share a
// destination.
//
// Grouping is what lets messages to the same server share a session while
// different providers stop waiting on each other. In relay mode there is
// nothing to group — every message takes the same single connection — so they
// all land in one group.
func (r *Runner) route(jobs []agent.Job) (map[string][]routedJob, []result) {
	groups := map[string][]routedJob{}
	var released []result
	now := time.Now()

	for _, job := range jobs {
		if r.mx == nil {
			groups[""] = append(groups[""], routedJob{job: job})
			continue
		}

		domain := policy.RecipientDomain(job.To)
		if domain == "" {
			released = append(released, result{Result: agent.Result{
				MessageID: job.MessageID,
				Status:    agent.StatusBounced,
				Error:     fmt.Sprintf("invalid recipient address: %q", job.To),
			}})
			continue
		}

		// This domain has been refusing us; hand the job back rather than
		// spend an attempt on it. Reporting it as failed would burn one of the
		// message's retries although nothing is wrong with the message.
		//
		// The remaining penalty goes with it. Without that the queue offers
		// the job again on the very next claim and a release-claim loop forms:
		// the penalty achieves nothing and only produces traffic.
		if penalty := r.throttle.RemainingPenalty(domain, now); penalty > 0 {
			released = append(released, result{Result: agent.Result{
				MessageID:         job.MessageID,
				Status:            agent.StatusReleased,
				RetryAfterSeconds: seconds(penalty),
			}})
			continue
		}

		groups[domain] = append(groups[domain], routedJob{
			job:        job,
			domain:     domain,
			maxPerConn: policy.LimitsFor(domain).MaxPerConn,
		})
	}

	return groups, released
}

// dispatch sends every group, each with its own set of connections.
//
// Concurrency within a group comes from the domain's own limits; the total is
// capped by the configured concurrency, so a batch spread over many domains
// cannot open more connections than the machine was told to.
func (r *Runner) dispatch(ctx context.Context, groups map[string][]routedJob, results chan<- result) {
	sem := make(chan struct{}, max(r.cfg.Concurrency, 1))
	var wg sync.WaitGroup

	for domain, group := range groups {
		workers := r.cfg.Concurrency
		if r.mx != nil {
			workers = policy.LimitsFor(domain).MaxConns
		}
		workers = clamp(workers, 1, len(group))

		queue := make(chan routedJob)
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for item := range queue {
					// The gap is waited out OUTSIDE the semaphore, and the
					// semaphore is taken per message. Held for a worker's
					// lifetime, workers pacing themselves for one provider
					// would sit on every slot while another provider's group
					// starved.
					if res, held := r.pace(ctx, item); held {
						results <- res
						continue
					}
					sem <- struct{}{}
					results <- r.sendOne(ctx, item)
					<-sem
				}
			}()
		}

		wg.Add(1)
		go func(group []routedJob, queue chan routedJob) {
			defer wg.Done()
			defer close(queue)
			for _, item := range group {
				select {
				case <-ctx.Done():
					// The lease ran out, or we are shutting down. These jobs
					// were never touched: reporting them as failed would burn
					// one of the message's attempts for nothing.
					results <- result{Result: agent.Result{
						MessageID:         item.job.MessageID,
						Status:            agent.StatusReleased,
						RetryAfterSeconds: windowClosedRetryAfter,
					}}
				case queue <- item:
				}
			}
		}(group, queue)
	}

	wg.Wait()
}

// pace holds the message back to the gap for its (sending domain, receiving
// infrastructure) pair.
//
// held=true means the message will not be sent and the result is a hand-back.
// When the slot does not fit inside the lease we do NOT sleep: sleeping past
// the window gets the work sent twice. The hand-back carries the wait, or the
// queue returns the job immediately and the release-claim loop forms.
//
// If the MX cannot be resolved no gap is applied: Deliver classifies that
// error properly, and pacing must not drop a delivery of its own accord.
func (r *Runner) pace(ctx context.Context, item routedJob) (result, bool) {
	if r.pacer == nil || r.mx == nil {
		return result{}, false
	}
	host, err := r.mx.PrimaryMX(ctx, item.domain)
	if err != nil {
		return result{}, false
	}

	now := time.Now()
	deadline, _ := ctx.Deadline()
	slot, ok := r.pacer.Reserve(
		policy.RecipientDomain(item.job.FromEmail), policy.MXIdentity(host), now, deadline)
	if !ok {
		return result{Result: agent.Result{
			MessageID:         item.job.MessageID,
			Status:            agent.StatusReleased,
			RetryAfterSeconds: seconds(slot.Sub(now)),
		}}, true
	}

	wait := slot.Sub(now)
	if wait <= 0 {
		return result{}, false
	}
	if !sleepCtx(ctx, wait) {
		return result{Result: agent.Result{
			MessageID:         item.job.MessageID,
			Status:            agent.StatusReleased,
			RetryAfterSeconds: windowClosedRetryAfter,
		}}, true
	}
	return result{}, false
}

// envelopeSender builds the Return-Path for a job.
//
// The message id goes inside the address (VERP), because a bounce arriving
// hours after the receiving server said 250 carries nothing else that would
// identify the message it belongs to.
//
// The domain is configuration, never a constant: it is what receiving servers
// run SPF against, and the connection comes from the customer's machine. A
// domain that does not authorise that machine turns a passing SPF into a
// failing one, so the right value is a subdomain of their own sending domain,
// with an MX pointing at the bounce collector. Empty is refused at startup.
func envelopeSender(job agent.Job, bounceDomain string) string {
	if bounceDomain == "" || job.MessageID == "" {
		return job.FromEmail
	}
	return "bounce+" + job.MessageID + "@" + bounceDomain
}

// seconds rounds a wait to whole seconds for the central system; never zero,
// which would mean "no wait at all".
func seconds(d time.Duration) int {
	if s := int(d.Round(time.Second) / time.Second); s > 0 {
		return s
	}
	return 1
}

// A failed report does not stop the batch: unreported work returns to the queue
// when the lease expires. Nothing is lost, but the message can go out twice —
// so the error is logged, never swallowed.
func (r *Runner) ackLoop(ctx context.Context, results <-chan result) counts {
	var total counts
	var pending []agent.Result

	ticker := time.NewTicker(ackInterval)
	defer ticker.Stop()

	flush := func() {
		for len(pending) > 0 {
			size := min(len(pending), ackMaxChunk)
			total.accepted += r.flushAck(ctx, pending[:size])
			pending = pending[size:]
		}
	}

	for {
		select {
		case res, ok := <-results:
			if !ok {
				flush()
				return total
			}
			total.add(res)
			pending = append(pending, res.Result)
			if len(pending) >= ackChunk {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (r *Runner) flushAck(ctx context.Context, chunk []agent.Result) int {
	ackCtx, cancel := context.WithTimeout(ctx, ackTimeout)
	defer cancel()

	resp, err := r.client.Ack(ackCtx, chunk)
	if err != nil {
		slog.Error("could not report results; the jobs return to the queue "+
			"when the lease expires", "count", len(chunk), "error", err)
		return 0
	}
	if resp.Accepted < len(chunk) {
		// The lease was exceeded and those jobs went to another node. Left
		// alone, the same work is sent again every round; a smaller batch or a
		// faster relay is the fix.
		slog.Warn("some results were not accepted (the lease may have expired)",
			"reported", len(chunk), "accepted", resp.Accepted)
	}
	slog.Debug("results reported", "count", len(chunk), "accepted", resp.Accepted)
	return resp.Accepted
}

func (r *Runner) sendOne(ctx context.Context, item routedJob) result {
	job, domain, maxPerConn := item.job, item.domain, item.maxPerConn

	msg, err := sender.Build(job, r.hostname)
	if err != nil {
		// A message that cannot be built will not build on a retry either.
		slog.Warn("could not build the message", "message_id", job.MessageID, "error", err)
		return result{Result: agent.Result{
			MessageID: job.MessageID,
			Status:    agent.StatusBounced,
			Error:     err.Error(),
		}}
	}

	// Signing is not optional. Unsigned mail from a domain whose DNS advertises
	// a DKIM key fails authentication outright, and with the envelope on the
	// bounce domain there is no aligned SPF left to carry DMARC either: the
	// message would be rejected, not merely filtered. Handing the job back is
	// the honest answer — it stays in the queue and goes out once the key is
	// available.
	signer, err := r.keys.For(job.FromEmail)
	if err != nil {
		slog.Error("cannot sign, holding the message back",
			"message_id", job.MessageID, "from", job.FromEmail, "error", err)
		return result{Result: agent.Result{
			MessageID:         job.MessageID,
			Status:            agent.StatusReleased,
			Error:             err.Error(),
			RetryAfterSeconds: unsignableRetryAfter,
		}}
	}
	if err := signer.Sign(msg); err != nil {
		// A signing failure is about the key, not the message. Not a bounce:
		// the address is fine.
		slog.Error("could not sign the message",
			"message_id", job.MessageID, "error", err)
		return result{Result: agent.Result{
			MessageID: job.MessageID,
			Status:    agent.StatusFailed,
			Error:     err.Error(),
		}}
	}

	if r.cfg.DryRun {
		// Released, not sent: a dry run exercises the protocol, and reporting
		// "sent" would mark real mail as delivered and drop it from the queue.
		slog.Info("dry-run: not sent, releasing",
			"message_id", job.MessageID, "recipient", job.To,
			"envelope", envelopeSender(job, r.cfg.BounceDomain))
		return result{Result: agent.Result{
			MessageID:         job.MessageID,
			Status:            agent.StatusReleased,
			RetryAfterSeconds: windowClosedRetryAfter,
		}}
	}

	outcome := r.transport.Deliver(ctx, mta.Delivery{
		EnvelopeFrom: envelopeSender(job, r.cfg.BounceDomain),
		To:           job.To,
		Domain:       domain,
		MaxPerConn:   maxPerConn,
		Data:         msg.Bytes(),
	})

	switch {
	case outcome.Delivered:
		if r.throttle != nil {
			r.throttle.Delivered(domain)
		}
		slog.Debug("sent", "message_id", job.MessageID, "recipient", job.To,
			"tls", outcome.TLS)
		return result{Result: agent.Result{
			MessageID: job.MessageID,
			Status:    agent.StatusSent,
		}}

	case outcome.Permanent:
		slog.Warn("permanent rejection", "message_id", job.MessageID,
			"recipient", job.To, "smtp_code", outcome.Code, "error", outcome.Err)
		return result{Result: agent.Result{
			MessageID: job.MessageID,
			Status:    agent.StatusBounced,
			Error:     outcome.Error(),
			SMTPCode:  outcome.Code,
		}}

	default:
		if r.throttle != nil {
			r.throttle.Deferred(domain, time.Now())
		}
		if outcome.SessionRejected() {
			// The server turned us away before it ever heard the recipient:
			// the address may be blocklisted, the PTR broken, the envelope
			// sender refused. Nothing to do with the recipient, so it is not a
			// bounce — but it is the earliest signal of a reputation problem,
			// counted separately and logged loudly.
			slog.Error("the server refused the session (possible blocklisting)",
				"message_id", job.MessageID, "domain", domain,
				"smtp_code", outcome.Code, "error", outcome.Err)
		} else {
			slog.Warn("send failed", "message_id", job.MessageID,
				"recipient", job.To, "smtp_code", outcome.Code, "error", outcome.Err)
		}
		return result{
			Result: agent.Result{
				MessageID: job.MessageID,
				Status:    agent.StatusFailed,
				Error:     outcome.Error(),
				SMTPCode:  outcome.Code,
			},
			sessionFailure: outcome.SessionRejected(),
		}
	}
}

func clamp(value, low, high int) int {
	return max(low, min(value, high))
}

// Exponential, capped at 60 seconds: if the central system is briefly down, the
// client must not drown it in requests.
func backoffDuration(failures int) time.Duration {
	const maxBackoff = 60 * time.Second
	return min(time.Duration(1<<min(failures, 6))*time.Second, maxBackoff)
}

// Returns false when the context is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// ---------------------------------------------------------------- preflight

// It is the only way a blocked node learns it has been unblocked, so it runs in
// both states. The interval comes from the server; requestPreflight wakes it
// early.
func (r *Runner) preflightLoop(ctx context.Context, interval time.Duration) {
	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-r.recheck:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}

		if ctx.Err() != nil {
			return
		}
		timer.Reset(r.runPreflight(ctx))
	}
}

// requestPreflight wakes the preflight loop without blocking. A second request
// while one is pending is a no-op — the point is "check soon", not "check N
// times".
func (r *Runner) requestPreflight() {
	select {
	case r.recheck <- struct{}{}:
	default:
	}
}

// Returns how long to wait before asking again.
//
// A failure here never stops sending: an outage on the central side must not
// become an outage in your mail. Nothing is lost by being generous, because
// claim still carries the verdict.
func (r *Runner) runPreflight(ctx context.Context) time.Duration {
	req := agent.PreflightRequest{
		Version:  r.client.Version(),
		Hostname: r.hostname,
		Probes:   r.runProbes(ctx),
		// Signing is mandatory now, so the answer to "do you sign" is always
		// yes. The identity is left out deliberately: the keys come from the
		// central system, one per verified domain, so it already knows more
		// about them than this process could report.
		DKIM:         agent.PreflightDKIM{Enabled: true},
		BounceDomain: r.cfg.BounceDomain,
	}

	resp, err := r.client.Preflight(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			return preflightRetryInterval
		}
		slog.Warn("health check failed; continuing to send", "error", err)
		return preflightRetryInterval
	}

	r.applyPreflight(resp)
	return clampInterval(time.Duration(resp.RecheckAfterSeconds) * time.Second)
}

func (r *Runner) runProbes(ctx context.Context) agent.PreflightProbes {
	var probes agent.PreflightProbes

	if relay, ok := r.transport.(prober); ok {
		relayOK, relayErr := preflight.CheckRelay(relay)
		probes.RelayReachable = &relayOK
		probes.RelayError = relayErr
		probes.RelayLocal = preflight.RelayIsLocal(r.cfg.SMTP.Host)
	} else {
		// Direct mode: there is no relay, so the reachability answer is left
		// unset rather than faked — the server treats a missing value as "not
		// measured". RelayLocal is true because the sending happens right
		// here, which is what makes this host's own port 25 decisive.
		probes.RelayLocal = true
	}

	r.mu.Lock()
	target, interval := r.port25Target, r.port25Interval
	due := time.Since(r.port25CheckedAt) >= interval
	r.mu.Unlock()

	if target != "" && due {
		status, errText := preflight.Port25(ctx, target, r.cfg.SMTP.Timeout)
		r.mu.Lock()
		r.port25Status, r.port25Error, r.port25CheckedAt = status, errText, time.Now()
		r.mu.Unlock()
	}

	r.mu.Lock()
	probes.Port25Egress, probes.Port25Error = r.port25Status, r.port25Error
	probes.Port25Target = r.port25Target
	r.mu.Unlock()
	return probes
}

// Both directions are logged loudly: the local log is the only signal the
// customer has, and "it silently stopped sending" is the worst outcome this
// feature could produce.
func (r *Runner) applyPreflight(resp *agent.PreflightResponse) {
	r.mu.Lock()
	r.port25Target = resp.Port25Probe.Target
	r.port25Interval = time.Duration(resp.Port25Probe.IntervalSeconds) * time.Second
	if r.port25Interval <= 0 {
		r.port25Interval = 6 * time.Hour
	}
	if r.port25Target == "" {
		// The probe was turned off; do not keep reporting a stale answer for a
		// probe that no longer runs.
		r.port25Status, r.port25Error = agent.Port25Skipped, ""
	}
	r.mu.Unlock()

	for _, warning := range resp.Warnings {
		slog.Warn("health check warning", "code", warning.Code, "detail", warning.Message)
	}

	wasBlocked := r.blocked.Swap(!resp.CanSend)
	switch {
	case !resp.CanSend:
		for _, blocker := range resp.Blockers {
			slog.Error("sending is blocked", "code", blocker.Code, "detail", blocker.Message)
		}
		if !wasBlocked {
			slog.Error("sending paused until the problems above are resolved",
				"recheck_in", time.Duration(resp.RecheckAfterSeconds)*time.Second)
		}
	case wasBlocked:
		slog.Info("sending resumed: the central system reports this node is healthy")
	default:
		slog.Debug("health check passed", "warnings", len(resp.Warnings))
	}
}

func clampInterval(interval time.Duration) time.Duration {
	return min(max(interval, minPreflightInterval), maxPreflightInterval)
}
